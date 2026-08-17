# 结课项目参考答案：个人知识助理（mini-agent 全链路整合）

> 对应任务书：[docs/tutorial/appendix-b-capstone.md](../../tutorial/appendix-b-capstone.md)。请先完成并自评，再对照本答案。
>
> 验证状态：以下全部代码已在 mini-agent 当前代码的副本上实测通过——`go build ./... && go vet ./... && go test ./...` 全绿（9 个新增测试全部 PASS）。加分项 P4 按 AGENTS.md 规则给出的是**经过验证的完整实现**，不是思路描述。

## 一、参考实现

### 改动总览

| 文件 | 动作 | 对应部分 |
| --- | --- | --- |
| `internal/agent/agent.go` | 改两处 + 加一个方法 | P3（接口化）、P1（恢复入口） |
| `internal/agent/session.go` | 新增 | P1 |
| `internal/agent/agent_test.go` | 新增 | P3 + P1 测试 |
| `internal/tools/gate.go` | 新增 | P2 |
| `internal/tools/gate_test.go` | 新增 | P2 测试 |
| `internal/rag/ingest_tool.go` | 新增 | P4 |
| `internal/rag/ingest_tool_test.go` | 新增 | P4 测试 |
| `cmd/agent/main.go` | 改四处 | 全部接线 |

### 1. `internal/agent/agent.go` 的改动

**改动 1：新增 `ChatClient` 接口，`client` 字段与 `New` 参数换成接口（P3 核心）**

在 `// Agent 持有一次任务的全部状态` 注释之前插入：

```go
// ChatClient 是 Agent 对 LLM 客户端的最小依赖（结课项目 P3：依赖倒置）。
// 生产实现是 *llm.Client；测试用脚本化假模型（agent_test.go 的 fakeModel）。
// 接口定义在使用方（agent 包）而非实现方，这是 Go 惯例——
// 需要多少方法就声明多少，*llm.Client 自动满足它，llm 包零改动。
type ChatClient interface {
	Chat(messages []llm.Message, tools []llm.Tool) (*llm.ChatResponse, error)
	ChatStream(messages []llm.Message, tools []llm.Tool, onDelta func(text string)) (*llm.ChatResponse, error)
}
```

然后两处类型替换（各一词）：

```go
// Agent 结构体里：
	client   ChatClient          // 原：client   *llm.Client

// New 签名：
func New(client ChatClient, registry *tools.Registry, systemPrompt string) *Agent {
	// 原：func New(client *llm.Client, ...)
```

注意：`ChatStream` 的第三个参数写成 `func(text string)`——与 `*llm.Client` 的实际方法签名（`client.go:104`）类型完全一致才能满足接口（参数名无所谓，类型必须同）。`compressIfNeeded` 里调的 `a.client.Chat` 不用动，接口里本来就有它。

**改动 2：`Messages()` 之后新增恢复入口（P1）**

```go
// RestoreMessages 用快照替换对话历史（结课项目 P1 会话恢复用）。
// 调用方必须保证快照合法：首条是 system、每条 tool 消息都有配对的
// assistant tool_calls——本方法不重复校验（LoadMessages 已做过结构校验）。
func (a *Agent) RestoreMessages(msgs []llm.Message) {
	a.messages = msgs
}
```

### 2. `internal/agent/session.go`（新增，P1）

```go
package agent

import (
	"encoding/json"
	"fmt"
	"mini-agent/internal/llm"
	"os"
	"path/filepath"
)

// SaveMessages 把对话历史原子落盘（结课项目 P1）。
//
// 原子写三步：写临时文件 → fsync → rename。崩溃最坏情况是留下一个 .tmp
// 文件，绝不会得到"写了一半的 session.json"——rename 在同一文件系统内是
// 原子操作（POSIX 保证）。这也是 vectorstore.Save 与 etcd/systemd 等
// 生产系统的同款手法。
func SaveMessages(path string, msgs []llm.Message) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(msgs, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	// 0600：对话历史是隐私数据，权限位与 SSH key 同级。
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil { // fsync：确保数据真正到盘，而不是停在页缓存
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// LoadMessages 读回会话快照。文件不存在时返回的错误可用 os.IsNotExist 判定
// （首次运行不是错误）；文件损坏或结构非法返回描述性错误——
// 落盘文件也是外部输入，不可信，必须校验。
func LoadMessages(path string) ([]llm.Message, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var msgs []llm.Message
	if err := json.Unmarshal(data, &msgs); err != nil {
		return nil, fmt.Errorf("会话文件损坏: %w", err)
	}
	if len(msgs) == 0 || msgs[0].Role != "system" {
		return nil, fmt.Errorf("会话文件非法：首条消息必须是 system")
	}
	return msgs, nil
}
```

### 3. `internal/agent/agent_test.go`（新增，P3 + P1 测试）

```go
package agent

import (
	"errors"
	"mini-agent/internal/llm"
	"mini-agent/internal/tools"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ===================== 脚本化假模型（结课项目 P3） =====================
//
// fakeModel 按"脚本"逐轮播放预定响应：第 1 轮返回 tool_calls，第 2 轮返回终稿……
// 确定性、零成本、离线可跑。它与 *llm.Client 满足同一个 ChatClient 接口——
// 契约由编译器检查（var _ ChatClient = ...），对真实 API 行为的假设则由
// llm 包的 httptest 测试钉住（分工见"关键设计点"）。

type fakeStep struct {
	resp *llm.ChatResponse
	err  error
}

type fakeModel struct {
	steps []fakeStep
	calls int
}

// 编译期契约断言：fakeModel 必须实现 ChatClient。
var _ ChatClient = (*fakeModel)(nil)

func (f *fakeModel) next() (*llm.ChatResponse, error) {
	f.calls++
	if len(f.steps) == 0 {
		return nil, errors.New("fake: 脚本已播完，被调次数超出预期")
	}
	s := f.steps[0]
	f.steps = f.steps[1:]
	return s.resp, s.err
}

func (f *fakeModel) Chat(_ []llm.Message, _ []llm.Tool) (*llm.ChatResponse, error) {
	return f.next()
}

func (f *fakeModel) ChatStream(_ []llm.Message, _ []llm.Tool, onDelta func(string)) (*llm.ChatResponse, error) {
	resp, err := f.next()
	if err == nil && onDelta != nil && len(resp.Choices) > 0 {
		// 模拟真实 client 的行为：终稿 content 走增量回调
		onDelta(resp.Choices[0].Message.Content)
	}
	return resp, err
}

func textResp(content string) *llm.ChatResponse {
	return &llm.ChatResponse{Choices: []llm.Choice{{
		Message:      llm.Message{Role: "assistant", Content: content},
		FinishReason: "stop",
	}}}
}

func toolCallResp(id, name, args string) *llm.ChatResponse {
	tc := llm.ToolCall{ID: id, Type: "function"}
	tc.Function.Name = name
	tc.Function.Arguments = args
	return &llm.ChatResponse{Choices: []llm.Choice{{
		Message:      llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{tc}},
		FinishReason: "tool_calls",
	}}}
}

// echoTool / boomTool：测试用最小工具，一个正常返回、一个永远失败。
type echoTool struct{}

func (echoTool) Name() string                     { return "echo" }
func (echoTool) Description() string              { return "原样回显参数，测试用" }
func (echoTool) ParametersSchema() map[string]any { return map[string]any{"type": "object"} }
func (echoTool) Execute(args string) (string, error) { return "echo: " + args, nil }

type boomTool struct{}

func (boomTool) Name() string                     { return "boom" }
func (boomTool) Description() string              { return "永远失败，测试用" }
func (boomTool) ParametersSchema() map[string]any { return map[string]any{"type": "object"} }
func (boomTool) Execute(string) (string, error)   { return "", errors.New("kaboom") }

// TestRunToolThenAnswer 钉住 ReAct 主路径：调一轮工具 → 拿到终稿，
// 且消息演化序列与 tool_call_id 回挂完全正确。
func TestRunToolThenAnswer(t *testing.T) {
	fake := &fakeModel{steps: []fakeStep{
		{resp: toolCallResp("call_1", "echo", `{"text":"hi"}`)},
		{resp: textResp("工具回显完成")},
	}}
	reg := tools.NewRegistry()
	reg.Register(echoTool{})
	ag := New(fake, reg, "测试系统提示")

	ans, err := ag.Run("打个招呼")
	if err != nil {
		t.Fatal(err)
	}
	if ans != "工具回显完成" {
		t.Errorf("ans=%q", ans)
	}

	msgs := ag.Messages()
	wantRoles := []string{"system", "user", "assistant", "tool", "assistant"}
	if len(msgs) != len(wantRoles) {
		t.Fatalf("len(msgs)=%d want %d: %+v", len(msgs), len(wantRoles), msgs)
	}
	for i, r := range wantRoles {
		if msgs[i].Role != r {
			t.Errorf("msgs[%d].Role=%s want %s", i, msgs[i].Role, r)
		}
	}
	if msgs[3].ToolCallID != "call_1" {
		t.Errorf("tool 消息必须回挂 tool_call_id，got %q", msgs[3].ToolCallID)
	}
	if !strings.Contains(msgs[3].Content, "echo:") {
		t.Errorf("tool 消息应包含工具结果，got %q", msgs[3].Content)
	}
	if fake.calls != 2 {
		t.Errorf("模型应被调用 2 次，实际 %d 次", fake.calls)
	}
}

// TestRunToolErrorFedBack 钉住"错误回喂"：工具失败后循环不中断，
// 错误以 tool 消息形式喂回模型，模型随后给出终稿。
func TestRunToolErrorFedBack(t *testing.T) {
	fake := &fakeModel{steps: []fakeStep{
		{resp: toolCallResp("call_1", "boom", `{}`)},
		{resp: textResp("工具失败，我换个思路")},
	}}
	reg := tools.NewRegistry()
	reg.Register(boomTool{})
	ag := New(fake, reg, "s")

	if _, err := ag.Run("试一下"); err != nil {
		t.Fatal(err)
	}
	msgs := ag.Messages()
	if len(msgs) != 5 || msgs[3].Role != "tool" {
		t.Fatalf("消息序列异常: %+v", msgs)
	}
	if !strings.Contains(msgs[3].Content, "tool error:") {
		t.Errorf("工具错误应回喂为 tool 消息，got %q", msgs[3].Content)
	}
}

// TestRunMaxSteps 钉住终止阀：模型反复调工具时，达到 MaxSteps 必须报错退出。
func TestRunMaxSteps(t *testing.T) {
	fake := &fakeModel{steps: []fakeStep{
		{resp: toolCallResp("c1", "echo", `{}`)},
		{resp: toolCallResp("c2", "echo", `{}`)},
	}}
	reg := tools.NewRegistry()
	reg.Register(echoTool{})
	ag := New(fake, reg, "s")
	ag.MaxSteps = 2

	if _, err := ag.Run("绕圈子"); err == nil || !strings.Contains(err.Error(), "最大步数") {
		t.Fatalf("应触发最大步数错误，got %v", err)
	}
}

// ===================== 会话持久化（结课项目 P1） =====================

func TestSessionRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "session.json") // sub 不存在，考验 MkdirAll
	msgs := []llm.Message{
		{Role: "system", Content: "s"},
		{Role: "user", Content: "我叫小王"},
	}
	if err := SaveMessages(path, msgs); err != nil {
		t.Fatal(err)
	}
	got, err := LoadMessages(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[1].Content != "我叫小王" {
		t.Fatalf("回读内容不符: %+v", got)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("会话文件权限应为 0600，got %v", fi.Mode().Perm())
	}
	// 原子写的副作用检查：rename 后不应残留临时文件
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("rename 后临时文件不应存在")
	}
}

func TestLoadMessagesCorrupt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadMessages(path); err == nil {
		t.Fatal("损坏的会话文件应报错而不是静默忽略")
	}
}
```

### 4. `internal/tools/gate.go`（新增，P2）

```go
package tools

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strings"
)

// ErrRejected 是审批被拒绝时的哨兵错误（含读取输入失败的 fail-closed 分支）。
// agent 循环会把它包进 "tool error: ..." 回喂给模型——模型读到后通常会
// 换方案或向用户解释，而不是盲目重试同一动作。
var ErrRejected = errors.New("用户拒绝了这次操作")

// ApprovalGate 包装任意 Tool，为高风险操作加一道人工确认闸门
// （Human-in-the-Loop 的单进程教学版，教程第 11 章概念的落地）。
//
// 模型看到的 Name/Description/Schema 与被包装工具完全一致——审批是纯粹的
// 客户端行为，不需要写进给模型看的说明书；它只需要在被拒绝时收到一条正常的
// 工具错误结果（错误回喂机制，第 2 章）。
type ApprovalGate struct {
	Inner Tool
	// In 必须与主输入循环共享同一个 *bufio.Reader：bufio 会预读，
	// 两个 buffered reader 抢同一个 stdin 会丢输入（阶段三 hitl-demo 踩过同款坑）。
	In *bufio.Reader
	// Out 是审批提示的输出。用 os.Stderr，别污染 stdout——
	// stdout 要留给模型回答（以及未来可能的协议帧，见 MCP 章节的 stdio 教训）。
	Out io.Writer
}

func (g *ApprovalGate) Name() string                     { return g.Inner.Name() }
func (g *ApprovalGate) Description() string              { return g.Inner.Description() }
func (g *ApprovalGate) ParametersSchema() map[string]any { return g.Inner.ParametersSchema() }

func (g *ApprovalGate) Execute(args string) (string, error) {
	fmt.Fprintf(g.Out, "\n[人工审批] 工具 %s 请求执行\n参数：%s\n输入 y 允许，其他输入拒绝: ",
		g.Name(), truncateRunes(args, 300))
	line, err := g.In.ReadString('\n')
	if err != nil {
		// 读不到输入（管道模式、EOF）按拒绝处理：fail-closed，宁可误拒不可误放
		return "", fmt.Errorf("%w（读取审批输入失败: %v）", ErrRejected, err)
	}
	if !strings.EqualFold(strings.TrimSpace(line), "y") {
		return "", ErrRejected
	}
	return g.Inner.Execute(args)
}

// truncateRunes 按 rune 截断，防中文截出乱码。参数可能很长（写文件的全文），
// 给人看的摘要只需要前几百个字符。
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…(已截断)"
}
```

### 5. `internal/tools/gate_test.go`（新增，P2 测试）

```go
package tools

import (
	"bufio"
	"errors"
	"io"
	"strings"
	"testing"
)

// spyTool 记录自己是否被执行，用来断言"被拒绝时内部工具没有被碰"。
type spyTool struct{ calls int }

func (s *spyTool) Name() string                     { return "spy" }
func (s *spyTool) Description() string              { return "记录调用次数，测试用" }
func (s *spyTool) ParametersSchema() map[string]any { return map[string]any{"type": "object"} }
func (s *spyTool) Execute(string) (string, error)   { s.calls++; return "done", nil }

func gateWithInput(input string) (*ApprovalGate, *spyTool) {
	spy := &spyTool{}
	g := &ApprovalGate{
		Inner: spy,
		In:    bufio.NewReader(strings.NewReader(input)),
		Out:   io.Discard,
	}
	return g, spy
}

func TestApprovalGateApprove(t *testing.T) {
	g, spy := gateWithInput("y\n")
	got, err := g.Execute(`{"x":1}`)
	if err != nil {
		t.Fatal(err)
	}
	if got != "done" || spy.calls != 1 {
		t.Errorf("批准后应执行内部工具：got=%q calls=%d", got, spy.calls)
	}
}

func TestApprovalGateReject(t *testing.T) {
	g, spy := gateWithInput("n\n")
	if _, err := g.Execute(`{"x":1}`); !errors.Is(err, ErrRejected) {
		t.Fatalf("拒绝应返回 ErrRejected，got %v", err)
	}
	if spy.calls != 0 {
		t.Error("被拒绝时内部工具不应执行")
	}
}

func TestApprovalGateEOFFailClosed(t *testing.T) {
	g, spy := gateWithInput("") // 立即 EOF（管道模式下最常见）
	if _, err := g.Execute(`{}`); !errors.Is(err, ErrRejected) {
		t.Fatalf("EOF 应按拒绝处理（fail-closed），got %v", err)
	}
	if spy.calls != 0 {
		t.Error("EOF 时内部工具不应执行")
	}
}
```

### 6. `internal/rag/ingest_tool.go`（新增，P4）

```go
package rag

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// KBIngest 让模型能把工作目录内的文本文件收录进知识库（结课项目 P4）。
//
// 与 /learn 斜杠命令的分工：/learn 由人显式发起；kb_ingest 把入库能力交给模型——
// "把 README 学了再回答我的问题"可以一条指令走完，这正是 agent 化的意义。
//
// 安全边界：与 file 工具同款路径防护（限 Root 内、拒绝绝对路径与 ../ 逃逸）；
// 只读取文件、不修改。知识写入是有副作用的操作（花 embedding 额度、改变后续
// 检索结果），接线时应外包一层 ApprovalGate（见 cmd/agent/main.go）。
type KBIngest struct {
	KB *KnowledgeBase
	// Root 是允许读取的根目录，与读写工具的工作目录保持一致。
	Root string
	// SavePath 是入库成功后的落盘路径；空串则不落盘
	// （不推荐：embedding 花了额度，进程退出就丢等于白花钱）。
	SavePath string
}

func (t *KBIngest) Name() string { return "kb_ingest" }

func (t *KBIngest) Description() string {
	return `把工作目录内的文本文件（md/txt 等）收录进知识库，收录后可用 kb_search 检索其内容。
当用户要求"学习/收录/记住"某个文档时使用。内容未变化的重复收录会被幂等跳过，不会产生重复块。`
}

func (t *KBIngest) ParametersSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{
				"type":        "string",
				"description": "相对工作目录的文件路径，如 notes/readme.md",
			},
		},
		"required": []string{"path"},
	}
}

func (t *KBIngest) Execute(args string) (string, error) {
	var p struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal([]byte(args), &p); err != nil {
		return "", fmt.Errorf("invalid tool arguments %q: %w", args, err)
	}
	full, err := t.resolve(p.Path)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(full)
	if err != nil {
		return "", fmt.Errorf("kb_ingest: 读取文件失败: %w", err)
	}
	// Ingest 自带 all-or-nothing 与幂等语义（内容相同返回 0, nil）
	n, err := t.KB.Ingest(p.Path, string(data))
	if err != nil {
		return "", err
	}
	if t.SavePath != "" {
		if err := t.KB.Store().Save(t.SavePath); err != nil {
			return "", fmt.Errorf("kb_ingest: %d 个块已入库但落盘失败: %w", n, err)
		}
	}
	return fmt.Sprintf("已收录 %s：新增 %d 个块（0 表示内容未变化被幂等跳过），知识库累计 %d 个块。",
		p.Path, n, t.KB.Store().Len()), nil
}

// resolve 与 tools.FileTool 的路径防护同构：拒绝绝对路径，
// Clean 之后必须仍落在 Root 内（防 ../ 逃逸）。
func (t *KBIngest) resolve(p string) (string, error) {
	if filepath.IsAbs(p) {
		return "", fmt.Errorf("kb_ingest: 不允许绝对路径: %q", p)
	}
	root := filepath.Clean(t.Root)
	full := filepath.Clean(filepath.Join(root, p))
	if full != root && !strings.HasPrefix(full, root+string(os.PathSeparator)) {
		return "", fmt.Errorf("kb_ingest: 路径越出工作目录: %q", p)
	}
	return full, nil
}
```

### 7. `internal/rag/ingest_tool_test.go`（新增，P4 测试）

```go
package rag

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mini-agent/internal/vectorstore"
)

// anyVecEmbedder 对任意文本返回固定向量——kb_test.go 的 fakeEmbedder 要求
// 每个文本预先登记，不适合这里（chunk 文本由 Chunk 决定）；本测试只关心
// 入库/幂等/路径防护的行为，不关心向量内容。
type anyVecEmbedder struct{}

func (anyVecEmbedder) Embed(texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range texts {
		v := make([]float32, 128)
		v[0] = 1
		out[i] = v
	}
	return out, nil
}

func TestKBIngestExecute(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "doc.md"),
		[]byte("向量数据库是一种按语义相似度检索的数据库。"), 0o644); err != nil {
		t.Fatal(err)
	}
	kb := NewKnowledgeBase(anyVecEmbedder{}, vectorstore.NewStore(), DefaultChunkOptions())
	tool := &KBIngest{KB: kb, Root: dir, SavePath: filepath.Join(dir, "kb.json")}

	out, err := tool.Execute(`{"path":"doc.md"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "已收录") || kb.Store().Len() == 0 {
		t.Fatalf("入库失败: out=%q len=%d", out, kb.Store().Len())
	}
	if _, err := os.Stat(filepath.Join(dir, "kb.json")); err != nil {
		t.Fatal("入库成功后应落盘 kb.json")
	}

	// 幂等：内容未变化的重复收录应被跳过（新增 0 块）
	out2, err := tool.Execute(`{"path":"doc.md"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out2, "新增 0 个块") {
		t.Errorf("重复收录应幂等跳过: %q", out2)
	}

	// 路径逃逸必须被拒绝
	if _, err := tool.Execute(`{"path":"../escape.md"}`); err == nil {
		t.Error("../ 逃逸应被拒绝")
	}
	if _, err := tool.Execute(`{"path":"/etc/passwd"}`); err == nil {
		t.Error("绝对路径应被拒绝")
	}
}
```

### 8. `cmd/agent/main.go` 的改动（四处）

**改动 1：常量区加快照路径**

```go
const (
	kbPath      = "./workspace/kb.json"
	memPath     = "memory.json"
	sessionPath = "./workspace/session.json" // 会话历史快照（结课项目 P1）
)
```

**改动 2：`main()` 开头建共享 reader，`write_file` 换 gated 注册（P2）**

```go
	// 全程序唯一的 stdin reader：主输入循环与审批闸门共享同一个。
	// bufio 会预读，两个 buffered reader 抢同一个 stdin 会丢输入。
	in := bufio.NewReader(os.Stdin)

	registry := tools.NewRegistry()
	registry.Register(tools.Calculator{})
	registry.Register(tools.HTTPFetch{})
	registry.Register(tools.NewReadFile("./workspace"))
	// write_file 是写操作：包一层审批闸门，模型请求写入时先由人确认（结课项目 P2）。
	// 模型看到的 schema 不变；被拒绝时它收到"用户拒绝了这次操作"的工具结果。
	registry.Register(&tools.ApprovalGate{
		Inner: tools.NewWriteFile("./workspace"),
		In:    in,
		Out:   os.Stderr,
	})
```

**改动 3：kb 接线块里注册 gated `kb_ingest`（P4）**（紧跟 `registry.Register(rag.NewKBSearch(...))` 之后）

```go
		// kb_ingest：把"收录文档"的能力也交给模型（结课项目 P4）。
		// 知识写入同样有副作用（花 embedding 额度、改变后续检索），也过审批闸门。
		registry.Register(&tools.ApprovalGate{
			Inner: &rag.KBIngest{KB: kb, Root: "./workspace", SavePath: kbPath},
			In:    in,
			Out:   os.Stderr,
		})
```

**改动 4：`ag` 创建后恢复会话；主循环从 `bufio.Scanner` 改为共享 reader，每轮成功后落盘（P1）**

```go
	// 会话恢复（结课项目 P1）：启动时载入上次的历史快照。
	// 文件不存在（首次运行）不是错误；损坏则提示后从新会话开始。
	if msgs, err := agent.LoadMessages(sessionPath); err == nil {
		ag.RestoreMessages(msgs)
		fmt.Printf("已恢复上次会话（%d 条消息）。\n", len(msgs))
	} else if !os.IsNotExist(err) {
		fmt.Println("会话快照读取失败（将从新会话开始）:", err)
	}

	fmt.Println("mini-agent 已启动，输入问题开始对话，输入 exit 退出。")
	for {
		fmt.Print("\n> ")
		line, err := in.ReadString('\n')
		if err != nil {
			break // EOF（Ctrl+D）
		}
		input := strings.TrimSpace(line)
		// ……（空行/exit//learn 分支与原样相同）……

		if _, err := ag.Run(input); err != nil {
			fmt.Println("出错:", err)
			continue
		}

		// 每轮成功结束后立刻落盘（第 9 章 checkpoint 思想：每完成一步存一步，
		// 进程崩溃/退出后下次启动可恢复到最近一轮）。
		if err := agent.SaveMessages(sessionPath, ag.Messages()); err != nil {
			fmt.Fprintln(os.Stderr, "保存会话失败:", err)
		}
	}
```

### 验证记录

上述代码在 mini-agent 当前代码副本上的实测输出（摘）：

```
$ go build ./... && go vet ./... && go test ./...
ok  mini-agent/internal/agent    # TestRunToolThenAnswer / TestRunToolErrorFedBack /
                                 # TestRunMaxSteps / TestSessionRoundTrip / TestLoadMessagesCorrupt
ok  mini-agent/internal/tools    # TestApprovalGateApprove / TestApprovalGateReject /
                                 # TestApprovalGateEOFFailClosed
ok  mini-agent/internal/rag      # TestKBIngestExecute（含幂等跳过、两类路径逃逸）
ok  mini-agent/internal/embed / memory / vectorstore   # 既有测试保持绿
```

## 二、关键设计点

1. **原子写为什么是三件套，缺一不可**。直接写 `session.json`：崩溃留半个 JSON，下次启动会话全废。写 `.tmp` 再 `rename`：rename 是原子替换，读方只见旧文件或新文件；`fsync` 防"rename 成功了但数据还在页缓存、掉电即丢"；`0600` 因为对话历史是隐私数据。易错：忘了 `f.Close()` 再 rename（Windows 上会直接失败，macOS/Linux 上埋下 fd 泄漏）。

2. **为什么只在 `Run` 成功后才落盘**。快照必须落在"user 边界"：一轮完整结束时的 messages 一定是 `…→assistant(终稿)` 收尾，恢复后合法。若在循环中途（比如工具刚执行完）保存，快照可能以孤儿 tool 消息收尾，恢复后 API 直接报错——这和第 3 章压缩切分点避开 tool 组是同一条纪律：**任何持久化/裁剪历史的操作，都要保证 tool_calls 配对完整**。

3. **审批闸门是装饰器，拒绝走错误回喂**。`ApprovalGate` 实现同一个 `Tool` 接口，`Registry`、`Agent`、模型三方都无感知。被拒绝时闸门返回 `ErrRejected`，`Run` 里现成的错误回喂路径把它包成 `tool error: 用户拒绝了这次操作` 的 tool 消息并正确回挂 `tool_call_id`——**协议完整性由 Run 统一保证，闸门绝不自己拼消息**。易错三形态：闸门里直接 `os.Exit`（agent 崩溃）、返回 `""` + nil（模型以为写入成功，后续行为全乱）、返回特殊文本让模型"猜"这是拒绝（不如 error 语义明确）。

4. **fail-closed：读不到输入 = 拒绝**。管道模式（`echo … | ./agent`）或 EOF 时 `ReadString` 返回错误——此时必须按拒绝处理，否则"无人在场的自动化"会静默放行写操作。这是第 11 章"超时默认拒绝"的单进程版。

5. **接口定义在使用方**。`ChatClient` 声明在 agent 包而不是 llm 包：llm 包不需要知道自己"被抽象了"，`Agent` 声明自己恰好需要的两个方法。这是 Go 与 Java 式接口前置的最大分歧点，面试常考。配套细节：`var _ ChatClient = (*fakeModel)(nil)` 编译期断言让"fake 没跟上接口演进"在编译期爆炸，而不是测试运行时才挂。

6. **fake 与真实 client 的契约分工**。编译器只保证"fake 和 `*llm.Client` 都满足接口"，不保证"fake 模仿得像真的"。后者由两层钉住：`llm` 包的 httptest 测试钉住真实 API 的线缆行为；`fakeModel.ChatStream` 里"终稿走 onDelta 回调"是显式模仿真实 client 的语义。测试金字塔：fake（循环逻辑）→ httptest（API 契约）→ 真实 key 冒烟（一句"1+1=?"）。

7. **KBIngest 复用而不是重写**。`Ingest` 已有幂等（sameChunks 短路）与 all-or-nothing，工具层只做"读文件 + 调用 + 落盘"。易错：绕过 `Ingest` 自己 chunk+add（丢掉幂等语义，重复收录块数翻倍）；或入库后不落盘（embedding 额度白花）。

8. **已知局限（诚实标注，面试主动讲）**：① 恢复的快照含旧 system prompt——若两次运行之间改了 prompt，恢复的是旧版（生产做法是快照带 prompt 版本或提供 `/new`）；② 快照不含 `usage` 累计，重启后从零计；③ 单实例假设——两个进程同时用同一个 `sessionPath` 会互踩（生产上文件锁或 SQLite）；④ `LoadMessages` 只校验"首条是 system"，不校验 tool 配对——由设计点 2 的"只在 user 边界落盘"保证，若允许外部导入快照则需加强校验。

## 三、对照清单

完成后逐条自评（与答案"一字不差"不是目标，覆盖这些条目才是）：

- [ ] `Agent` 依赖的是接口而非 `*llm.Client`，`main.go` 传参零改动仍编译通过
- [ ] 三个内核测试覆盖：工具调用后终止（含消息序列与 tool_call_id 断言）、错误回喂、MaxSteps 终止
- [ ] 测试零网络依赖（断网可跑），且有编译期接口断言
- [ ] 会话文件原子写（tmp+rename）、0600 权限、损坏文件启动不崩
- [ ] 只在 Run 成功后落盘，并能说清"为什么快照不能落在 tool 组中间"
- [ ] 审批拒绝返回 error 走错误回喂，agent 不崩、模型收到"被拒绝"的工具结果
- [ ] 审批读输入失败（EOF/管道）时默认拒绝
- [ ] 全程序共享同一个 stdin 的 `*bufio.Reader`，并能讲出预读坑
- [ ] 审批提示输出到 stderr 而非 stdout
- [ ] kb_ingest：路径逃逸拒绝、重复收录幂等、入库后落盘
- [ ] 能口头讲清 design point 5（接口位置）与 6（契约分工）——这两条是面试高概率追问
