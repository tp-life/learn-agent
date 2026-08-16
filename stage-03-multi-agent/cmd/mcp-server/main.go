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
// 代价：只能本地用，无法远程共享——远程场景才需要 HTTP+SSE。
//
// 【stdio server 的第一坑】：stdout 是协议信道，只能是 JSON-RPC 消息；
// 任何日志/调试输出都必须写 stderr，否则 client 解析直接炸。
// 本项目 mini-agent 的 Registry.Call 与 Calculator.Execute 里留有 fmt.Println
// 调试输出（阶段一写的），tools/call 时必须处理——见下方 TODO(练习7) 的提示。
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"

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
// 练习 7 的 tools/call 会把 os.Stdout 临时换走（拦截工具里的调试 Println），
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

// ok 构造成功响应。
func ok(req *Request, result any) *Response {
	return &Response{JSONRPC: "2.0", ID: req.ID, Result: result}
}

// rpcErr 构造协议层错误响应。
func rpcErr(req *Request, code int, msg string) *Response {
	return &Response{JSONRPC: "2.0", ID: req.ID, Error: &RPCError{Code: code, Message: msg}}
}

// ============================ 练习区（由学习者完成） ============================
//
// TODO(练习7): MCP method dispatch 与四个 handler
//
// 任务：实现 method 路由函数与各 handler，替换下面的 stub：
//
//   func dispatch(reg *api.Registry, req *Request) *Response
//
// 需要支持的方法（返回 nil 表示"这是通知，不回复"）：
//   1. initialize → result 三件套：protocolVersion（用常量）/
//      capabilities.tools（空对象即可，表示"支持 tools 原语"）/
//      serverInfo{name, version}
//   2. notifications/initialized → 纯通知（无 id），不回复
//   3. tools/list → 从 Registry 生成 [{name, description, inputSchema}]，
//      包在 result.tools 里。inputSchema 就是工具的 ParametersSchema()
//      ——与 function calling 的 tool schema 几乎同构不是巧合：
//      MCP client 拿到后包一层就能喂模型，这正是
//      "MCP 工具最终转成 function calling schema"的接口形状
//   4. tools/call → params 为 {name: string, arguments: object}；
//      arguments 是 object 而 Registry.Call 吃 JSON 字符串，
//      桥接方式就是 re-marshal；结果包成
//      {content: [{type:"text", text: ...}]}
//
// 提示：
//   - 通知的判定：len(req.ID) == 0；未知【通知】也要静默吞掉，未知【请求】
//     才回 rpcErr(req, codeMethodNotFound, ...)
//   - Registry.Schemas() 返回 []api.ToolSchema（function calling 格式），
//     字段在 .Function.{Name,Description,Parameters} 下；它内部遍历 map，
//     顺序随机——sort.Slice 按名字排一下，输出才稳定可比对
//   - 工具业务错误【不】用 RPC error 返回：包成正常 result 加 "isError": true，
//     text 放 err.Error()。RPC error 是"协议层失败"，工具失败是业务结果，
//     client 会把它喂回模型让它自我纠正（与 agent.go"工具错误喂回模型"同一思想）
//   - 【大坑】Registry.Call 和 Calculator.Execute 里有 fmt.Println 调试输出，
//     直接调会污染 stdout 协议信道。建议包一个 callTool(reg, name, args)，
//     调用前 os.Stdout = os.Stderr，defer 恢复 protocolOut
//     （单线程 dispatch 下换全局变量是安全的）
//   - params 畸形（不是合法 JSON object）用 -32602 invalid params
//
// 验收（在 stage-03-multi-agent/ 下）：
//
//   printf '%s\n' \
//     '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}' \
//     '{"jsonrpc":"2.0","method":"notifications/initialized"}' \
//     '{"jsonrpc":"2.0","id":2,"method":"tools/list"}' \
//     '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"calculator","arguments":{"expression":"1+2*3"}}}' \
//     | go run ./cmd/mcp-server
//
// 应得到恰好 3 行响应（通知无回复）：id:1 握手三件套、id:2 四个工具、
// id:3 算出 "7"。再试 '1/0' 应得 isError:true，试未知方法应得 -32601。
//
// 参考答案：docs/solutions/stage-03/exercise-7-mcp-server.md（完成后再看）
func dispatch(reg *api.Registry, req *Request) *Response {
	// stub：一切方法都回 method not found，保证骨架可编译、可运行。
	return rpcErr(req, codeMethodNotFound, "method not found: "+req.Method)
}
