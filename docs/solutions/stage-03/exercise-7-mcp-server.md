# 练习 7 参考答案：MCP stdio server

> 对应 TODO：`stage-03-multi-agent/cmd/mcp-server/main.go` 的 `TODO(练习7)`。
> **完成练习并自评后再看本文档。**
> 本文档基础实现代码已于 2026-08-14 实际粘贴进 `stage-03-multi-agent/cmd/mcp-server/main.go` 验证：
> `go vet ./cmd/mcp-server/`、`go build ./cmd/mcp-server/` 通过；用管道实测 JSON-RPC 交互（命令与真实输出见文末"验证记录"），
> initialize / tools/list / tools/call（`1+2*3` 正确算出 `7`）/ 未知方法 / 解析错误 / 工具业务错误（isError）全部符合预期。
> 进阶实现（ping，见第三节）同日随基础版一起实测通过。
> 验证后项目已恢复骨架版——答案只存在于本文档。

---

## 一、参考实现

### `stage-03-multi-agent/cmd/mcp-server/main.go`（完整实现，含骨架已有部分以便对照）

骨架已提供的部分（文件头注释、JSON-RPC 类型、main 读取循环、注册表组装）与参考答案一致，
练习要补的是 `dispatch` 与四个 handler、`callTool`、`ok`/`rpcErr` 两个helper。

```go
// Package main 把 mini-agent 的工具以 MCP（Model Context Protocol）stdio server 暴露。
//
// ============================ MCP 是什么（面试考点） ============================
//
// MCP 解决"工具接入标准化"：没有 MCP 时，N 个 agent 框架 × M 个工具 = N×M 种接法；
// 有了 MCP，工具方实现一次 server，任何 MCP client（Claude、IDE、自研 agent）都能用
// —— 类似 USB-C 统一了充电口（教程第二节第 8 条）。
//
// 架构三角：
//   - server：暴露能力的一方（本程序）；
//   - client：消费能力的一方，通常内嵌在 agent 里；
//   - host：承载 client 的应用（如 Claude Desktop、IDE、我们的编排器）。
//
// 三原语：
//   - tools：可执行的函数（≈ function calling 的工具）——本练习只实现这个；
//   - resources：可读的数据（≈ 文件/文档）；
//   - prompts：预置 prompt 模板。
//
// MCP 与 function calling 是【不同层】（教程 3.1 Q9 的口径）：
// function calling 是"模型 ↔ 应用"之间模型请求执行工具的机制；
// MCP 是"应用 ↔ 工具提供方"之间的标准化接入协议。
// 典型链路：MCP client 列出 server 的 tools → 转成 function calling schema 给模型
// → 模型选中 → client 通过 MCP 调 server 执行。
// 本练习做的是这条链路的【右半边】（工具提供方）；tools/list 的输出与
// function calling 的 tool schema 几乎同构，正是为了这一步转换零成本。
//
// ============================ 为什么选 stdio 传输 ============================
//
// MCP 常用两种传输：stdio 与 HTTP+SSE。选 stdio 的原因：
//   - 本地进程模型：client 按需拉起 server 子进程，生命周期跟随 client，免部署；
//   - 免鉴权：信道是本机管道，不暴露网络端口，天然没有鉴权问题；
//   - 协议简单：一行一个 JSON-RPC 2.0 消息（newline-delimited JSON），无 HTTP 框架。
// 代价：只能本地用，无法远程共享——远程场景才需要 HTTP+SSE（见参考答案进阶节）。
//
// 【stdio server 的第一坑】：stdout 是协议信道，只能是 JSON-RPC 消息；
// 任何日志/调试输出都必须写 stderr，否则 client 解析直接炸。
// 本项目 mini-agent 的 Registry.Call 与 Calculator.Execute 里留有 fmt.Println
// 调试输出（阶段一写的），tools/call 时必须把 os.Stdout 临时接管——见 callTool。
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"sort"

	"mini-agent/api"
)

// ============================ JSON-RPC 2.0 线缆类型 ============================
//
// MCP 的消息层就是 JSON-RPC 2.0：Request（有 id，要回复）/
// Notification（无 id，不回复）/ Response（回显 id，result 与 error 二选一）。
// 规范错误码：-32700 parse error、-32601 method not found。

// Request 是一条 JSON-RPC 2.0 请求（或通知）。
type Request struct {
	JSONRPC string `json:"jsonrpc"`
	// ID 用 RawMessage 原样保存：JSON-RPC 的 id 可以是数字或字符串，
	// 回显时必须原样奉还；通知（notification）没有这个字段，len(ID)==0 即通知。
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method"`
	// Params 延迟解析：不同 method 的 params 结构不同，dispatch 后再各自 unmarshal。
	Params json.RawMessage `json:"params,omitempty"`
}

// Response 是一条 JSON-RPC 2.0 响应。Result 与 Error 互斥（omitempty 保证只出现一个）。
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// RPCError 是 JSON-RPC 2.0 的错误对象。
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

const (
	codeParseError     = -32700
	codeMethodNotFound = -32601

	// protocolVersion 是本 server 声明支持的 MCP 协议版本（initialize 时告知 client）。
	protocolVersion = "2024-11-05"
	serverName      = "mini-agent-mcp"
	serverVersion   = "0.1.0"
)

// protocolOut 持有程序启动时的【真 stdout】。
// tools/call 期间 os.Stdout 会被临时换走（拦截工具里的调试 Println），
// 但协议响应必须始终写真 stdout，所以启动时先把它存下来。
var protocolOut = os.Stdout

func main() {
	// 日志一律走 stderr——stdout 是协议信道，一个字节都不能污染。
	logf("mcp-server starting (pid %d)", os.Getpid())

	// 组装工具注册表：直接复用 mini-agent 内核的门面（api 包）。
	// read_file/write_file 的 root 把文件操作限制在 workspace/ 内。
	workspace := "workspace"
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "mkdir workspace: %v\n", err)
		os.Exit(1)
	}
	reg := api.NewRegistry()
	reg.Register(api.Calculator{})
	reg.Register(api.HTTPFetch{})
	reg.Register(api.NewReadFile(workspace))
	reg.Register(api.NewWriteFile(workspace))

	// stdin 行读取循环：MCP stdio 传输是 newline-delimited JSON，
	// 一行一条消息，读完一行处理一行回一行。
	out := bufio.NewWriter(protocolOut)
	scanner := bufio.NewScanner(os.Stdin)
	// 工具参数可能很大（如 write_file 的 content），放宽默认 64KB 的行上限。
	scanner.Buffer(make([]byte, 0, 1024*1024), 16*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var req Request
		if err := json.Unmarshal(line, &req); err != nil {
			// parse error：连 id 都解析不出来，按规范回 id:null。
			writeResponse(out, &Response{
				JSONRPC: "2.0",
				ID:      nil, // nil RawMessage 序列化为 null
				Error:   &RPCError{Code: codeParseError, Message: "parse error: " + err.Error()},
			})
			continue
		}
		resp := dispatch(reg, &req)
		if resp == nil {
			continue // 通知（notification）：协议规定不回复
		}
		writeResponse(out, resp)
	}
	if err := scanner.Err(); err != nil {
		logf("stdin read error: %v", err)
	}
	logf("stdin closed, exit")
}

// logf 写 stderr 的服务端日志。
func logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "[mcp-server] "+format+"\n", args...)
}

// writeResponse 把一条响应写进协议信道并立即 flush（client 可能阻塞等回复）。
func writeResponse(out *bufio.Writer, resp *Response) {
	resp.ID = normID(resp.ID)
	if err := json.NewEncoder(out).Encode(resp); err != nil {
		logf("encode response: %v", err)
		return
	}
	if err := out.Flush(); err != nil {
		logf("flush: %v", err)
	}
}

// normID 把空 id 归一成 nil（序列化为 null），避免输出 "id":null 之外的怪形状。
func normID(id json.RawMessage) json.RawMessage {
	if len(id) == 0 {
		return nil
	}
	return id
}

// dispatch 按 method 路由到各 handler。
// 返回 nil 表示这是通知，不需要回复。
func dispatch(reg *api.Registry, req *Request) *Response {
	// 通知没有 id，按 JSON-RPC 规范绝不回复。
	isNotification := len(req.ID) == 0

	switch req.Method {
	case "initialize":
		// MCP 握手：client 问"你是谁、支持什么协议、有哪些能力"。
		return ok(req, map[string]any{
			"protocolVersion": protocolVersion,
			"capabilities": map[string]any{
				// 声明支持 tools 原语；resources/prompts 不声明即表示不支持。
				"tools": map[string]any{},
			},
			"serverInfo": map[string]any{
				"name":    serverName,
				"version": serverVersion,
			},
		})

	case "notifications/initialized":
		// client 完成握手后的告知，是纯通知。
		return nil

	case "tools/list":
		// 把 Registry 里的工具转成 MCP 格式：{name, description, inputSchema}。
		// 注意这与 function calling 的 tool schema（{type:"function", function:{...}}）
		// 几乎同构——MCP client 拿到后包一层就能喂给模型，这正是
		// "MCP 工具最终转成 function calling schema"的接口形状。
		schemas := reg.Schemas()
		// map 遍历顺序随机，排序保证输出稳定（方便测试与对比）。
		sort.Slice(schemas, func(i, j int) bool {
			return schemas[i].Function.Name < schemas[j].Function.Name
		})
		tools := make([]map[string]any, 0, len(schemas))
		for _, s := range schemas {
			tools = append(tools, map[string]any{
				"name":        s.Function.Name,
				"description": s.Function.Description,
				"inputSchema": s.Function.Parameters,
			})
		}
		return ok(req, map[string]any{"tools": tools})

	case "tools/call":
		// 参数：{name: string, arguments: object}。
		// 注意 arguments 是 object（MCP 的形状），而 Registry.Call 吃 JSON 字符串
		// （mini-agent 的形状）——桥接工作就是 re-marshal。
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return rpcErr(req, -32602, "invalid params: "+err.Error())
		}
		args := string(p.Arguments)
		if len(p.Arguments) == 0 {
			args = "{}"
		}
		result, err := callTool(reg, p.Name, args)
		if err != nil {
			// 工具业务错误【不】用 RPC error 返回，而是包成正常 result + isError:true。
			// 原因：RPC error 表示"协议层失败"（方法不存在、参数畸形），
			// 工具执行失败是业务结果的一部分——client 会把它喂回模型，
			// 让模型看到错误并自我纠正（与 mini-agent agent.go 里
			// "工具错误喂回模型"是同一思想）。
			return ok(req, map[string]any{
				"content": []map[string]any{
					{"type": "text", "text": err.Error()},
				},
				"isError": true,
			})
		}
		return ok(req, map[string]any{
			"content": []map[string]any{
				{"type": "text", "text": result},
			},
		})

	default:
		if isNotification {
			return nil // 未知通知：静默吞掉，不回复
		}
		return rpcErr(req, codeMethodNotFound, "method not found: "+req.Method)
	}
}

// callTool 调 Registry.Call，期间把 os.Stdout 临时换成 stderr：
// mini-agent 的 Registry.Call / Calculator.Execute 里有 fmt.Println 调试输出，
// 不拦截就会污染协议信道（stdio server 的第一坑在本项目的具体体现）。
// 单线程 dispatch 下换全局变量是安全的；协议响应走 protocolOut 不受影响。
func callTool(reg *api.Registry, name, args string) (string, error) {
	os.Stdout = os.Stderr
	defer func() { os.Stdout = protocolOut }()
	return reg.Call(name, args)
}

// ok 构造成功响应。
func ok(req *Request, result any) *Response {
	return &Response{JSONRPC: "2.0", ID: req.ID, Result: result}
}

// rpcErr 构造协议层错误响应。
func rpcErr(req *Request, code int, msg string) *Response {
	return &Response{JSONRPC: "2.0", ID: req.ID, Error: &RPCError{Code: code, Message: msg}}
}
```

## 二、关键设计点

1. **日志必须走 stderr，这是 stdio server 的第一坑**。stdout 是协议信道，client 对它做逐行 JSON 解析，混进任何非 JSON 字节（一条 log、一个 `fmt.Println` 调试输出）就直接解析失败。**本项目有具体实例**：mini-agent 的 `Registry.Call` 和 `Calculator.Execute` 里留着阶段一的 `fmt.Println` 调试输出（`internal/tools/tools.go:73`、`calculator.go:42`），又不允许为 MCP 改 mini-agent 的代码，所以参考实现在 `callTool` 里把 `os.Stdout` 临时换成 `os.Stderr`，同时在程序启动时把真 stdout 存进 `protocolOut` 供协议响应专用。**易错处**：只存 `protocolOut` 不换 `os.Stdout`，工具里的 Println 照样污染信道；只换不存，响应就写到 stderr 去了，client 永远等不到回复。

2. **tools/call 的业务错误不用 RPC error，用 `isError: true`**。RPC error 表达"协议层失败"（方法不存在 -32601、参数畸形 -32602），工具执行失败（算式非法、路径越界、HTTP 抓取失败）是正常的业务结果——MCP client 会把它作为工具输出喂回模型，让模型看到错误并自我纠正，与 mini-agent `agent.go` 里"工具错误喂回模型"是同一思想。**易错处**：把工具错误塞进 RPC error，模型这一侧表现为"调用失败"而非"工具返回了错误信息"，自我纠正的回路就断了；实测 `1/0` 时返回 `{"content":[{"text":"invalid or unsafe expression: ..."}],"isError":true}` 才是对的形状。

3. **MCP 与 function calling 是不同层**（教程 3.1 Q9）：function calling 是"模型 ↔ 应用"的机制，MCP 是"应用 ↔ 工具提供方"的协议。tools/list 的输出 `{name, description, inputSchema}` 与 Registry.Schemas() 的 function calling 格式几乎同构不是巧合——MCP client 拿到工具列表后包一层 `{type:"function", function:{...}}` 就能喂给模型。本练习做的就是这条链路的右半边；面试能画出"MCP client 列工具 → 转 schema 喂模型 → 模型选中 → client 调 tools/call"这条链路就到位了。

4. **Request.ID 用 `json.RawMessage` 原样回显**：JSON-RPC 的 id 可以是数字也可以是字符串，定义成 `int` 会把字符串 id 的请求解析失败，定义成 `any` 会把数字 `1` 回显成 `1` 但类型信息要手工维护；RawMessage 零解析、原样奉还，同时 `len(ID)==0` 天然就是"通知"的判定条件。

5. **tools/list 排序**：`Registry.Schemas()` 内部遍历 map，顺序随机；每次列出不同顺序的工具列表对 client 无害，但会让测试和人工比对很烦，`sort.Slice` 一行解决。

6. **已知坑（不属于本练习修改范围）**：mini-agent `internal/tools/file.go` 的 `resolve` 用了 `os.PathListSeparator`（macOS 上是 `:`）做路径前缀判断，应为 `os.PathSeparator`（`/`），导致 read_file/write_file 的几乎所有路径都被判"越出工作目录"。MCP 层如实透传这个错误（`isError:true`），协议行为本身正确；这是 mini-agent 练习 4 代码的 bug，需回到 mini-agent 修复，本练习不动它。

## 三、进阶实现（ping）

### 取舍说明

- **ping 值得实现**：MCP client 会用 ping 做保活/健康检查（尤其长连接 client），实现只需一个 case，且能立刻用管道验证。
- **resources/prompts 本练习不实现（本项为开放讨论，无需实现）**：理由是三原语中只有 tools 与"把 mini-agent 工具暴露出去"的目标直接相关；resources 暴露的是"可读数据"（我们的 workspace 文件、知识库文档可以做，但 mini-agent 侧没有统一的资源抽象，做了也只是玩具），prompts 暴露预置模板（mini-agent 的 system prompt 是写死的，没有模板管理）。initialize 里不声明这两个 capability，合规 client 就不会来问——这正是 capability 协商的意义。如果面试被问"三原语为什么只实现 tools"，按上面的"目标相关性 + capability 声明"回答即可。

### ping 实现（已于 2026-08-14 随基础版实测通过）

在 `dispatch` 的 switch 中增加一个 case：

```go
	case "ping":
		// 保活探测：回一个空 result 即可。
		return ok(req, map[string]any{})
```

验证（真实输出）：

```bash
printf '%s\n' '{"jsonrpc":"2.0","id":6,"method":"ping"}' | go run ./cmd/mcp-server
# {"jsonrpc":"2.0","id":6,"result":{}}
```

## 四、对照清单

完成后逐条自评（不要求与答案一字不差，覆盖条目即可）：

- [x] `initialize` 返回 `protocolVersion:"2024-11-05"`、`capabilities.tools`、`serverInfo{name,version}` 三件套
- [x] `notifications/initialized`（及一切无 id 的通知）不产生任何响应行
- [x] `tools/list` 输出 4 个工具（calculator/http_fetch/read_file/write_file），每项含 `name/description/inputSchema`，`inputSchema` 就是工具的 `ParametersSchema()`
- [x] `tools/call` 把 `arguments`（object）桥接成 JSON 字符串交给 `Registry.Call`，结果包成 `content:[{type:"text",text:...}]`；`1+2*3` 实测返回 `7`
- [x] 工具业务错误（如 `1/0`、未知工具名）返回正常 result + `isError:true`，不是 RPC error
- [x] 未知 method 回 `-32601`；畸形 JSON 回 `-32700` 且 `id:null`
- [x] 工具的调试 `fmt.Println` 输出不出现在 stdout（用 `2>/dev/null` 后输出仍是纯 JSON-RPC 行）
- [x] `go vet ./cmd/mcp-server/` 与 `go build ./...` 通过

加分项（做了才需要勾，参考"进阶实现"一节）：

- [x] 实现了 `ping` 并实测返回空 result
- [x] 能口头回答：为什么不实现 resources/prompts？MCP 与 function calling 的分层关系？

## 验证记录（2026-08-14 实测）

命令（在 `stage-03-multi-agent/` 下）：

```bash
printf '%s\n' \
  '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}' \
  '{"jsonrpc":"2.0","method":"notifications/initialized"}' \
  '{"jsonrpc":"2.0","id":2,"method":"tools/list"}' \
  '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"calculator","arguments":{"expression":"1+2*3"}}}' \
  | go run ./cmd/mcp-server
```

真实输出摘要（stdout，共 3 行——通知无回复）：

```json
{"jsonrpc":"2.0","id":1,"result":{"capabilities":{"tools":{}},"protocolVersion":"2024-11-05","serverInfo":{"name":"mini-agent-mcp","version":"0.1.0"}}}
{"jsonrpc":"2.0","id":2,"result":{"tools":[{"description":"计算数学表达式。…","inputSchema":{…"expression"…},"name":"calculator"},{"…","name":"http_fetch"},{"…","name":"read_file"},{"…","name":"write_file"}]}}
{"jsonrpc":"2.0","id":3,"result":{"content":[{"text":"7","type":"text"}]}}
```

stderr（日志与工具调试输出都被正确隔离）：

```
[mcp-server] mcp-server starting (pid 28558)
这里在执行真正的本地代码 calculator {"expression":"1+2*3"}
这里是实际调用的地方： {"expression":"1+2*3"}
[mcp-server] stdin closed, exit
```

异常路径实测（均符合预期）：

- `1/0` → `{"content":[{"text":"invalid or unsafe expression: eval:1:3: invalid operation: division by zero",...}],"isError":true}`
- 未知方法 `no/such` → `{"error":{"code":-32601,"message":"method not found: no/such"}}`
- 非 JSON 行 `not-json` → `{"id":null,"error":{"code":-32700,"message":"parse error: ..."}}`
- `ping`（进阶）→ `{"jsonrpc":"2.0","id":6,"result":{}}`
- read_file/write_file → `isError:true` + "路径越出工作目录"（mini-agent file.go 的 `os.PathListSeparator` bug，见关键设计点 6）
