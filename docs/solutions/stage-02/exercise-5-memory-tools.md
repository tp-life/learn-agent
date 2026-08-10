# 练习 5 参考答案：长期 memory 工具

> 对应 TODO：`mini-agent/internal/memory/memory.go` 的三处 `TODO(练习5)`（Remember / Recall / 两个工具的 Execute + main.go 注册）。
> **完成练习并自评后再看本文档。**
> 本文档代码已于 2026-08-06 实际粘贴进项目验证：临时应用练习 2 参考实现后，`go vet ./...` 通过，`go test ./internal/memory/ -v` 8 个测试全部通过（验证后已恢复 vectorstore 与 memory 为骨架版，diff 逐字节一致）。
> 2026-08-06 进阶回补：新增"三、进阶实现"一节（记忆去重与遗忘，对应原"关键设计点"第 5 条的已知局限），代码在 `/tmp` 项目副本中实测验证（memory 包 8 基础 + 5 进阶、vectorstore 包 3 个新增测试全绿，`go vet ./...` 通过），不进项目代码树——项目里请自己实现。

---

## 一、参考实现

### `internal/memory/memory.go`（只给出需要实现的部分；骨架的包注释、Embedder、Store/NewStore、两个工具的说明书不变）

import 从骨架的 `errors` 改为：

```go
import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"mini-agent/internal/tools"
	"mini-agent/internal/vectorstore"
)
```

```go
// Remember 把一条事实写入长期记忆并立即持久化：embed → 入库 → Save 落盘。
func (s *Store) Remember(fact string) error {
	// 空事实没有语义，在最早的层面拦下——embed.Client 也会拒绝空串，
	// 但让坏输入多走一层没有意义。
	fact = strings.TrimSpace(fact)
	if fact == "" {
		return fmt.Errorf("memory: empty fact")
	}

	// embed 是批量接口，单条也要包成单元素切片。
	vecs, err := s.emb.Embed([]string{fact})
	if err != nil {
		return fmt.Errorf("memory: embed fact: %w", err)
	}

	doc := vectorstore.Document{
		// ID 只需唯一：纳秒时间戳足够（单进程内不会重复）。
		ID:       fmt.Sprintf("mem-%d", time.Now().UnixNano()),
		Text:     fact,
		Vector:   vecs[0],
		Metadata: map[string]string{"kind": "memory"},
	}
	if err := s.vs.Add(doc); err != nil {
		return fmt.Errorf("memory: add fact: %w", err)
	}

	// 长期记忆的价值就在"跨会话"：写内存不落盘等于没记。
	// 每次写入即全量 Save，数据量小（几十上百条）时完全够用；
	// 保存失败必须返回错误，不能吞——否则用户以为记住了，重启后丢失。
	if err := s.vs.Save(s.path); err != nil {
		return fmt.Errorf("memory: persist: %w", err)
	}
	return nil
}

// Recall 用自然语言查询检索相关记忆，返回事实文本列表（按相关度降序）。
func (s *Store) Recall(query string, topK int) ([]string, error) {
	// topK<=0 兜底为默认值而不是报错：本层的调用方是工具 Execute，
	// 模型很可能不传 top_k——面向模型的边界层宜宽，
	// 面向代码的底层库（vectorstore.Search 对 topK<=0 报错）宜严。
	if topK <= 0 {
		topK = 3
	}

	vecs, err := s.emb.Embed([]string{query})
	if err != nil {
		return nil, fmt.Errorf("memory: embed query: %w", err)
	}

	// 空库时 Search 返回空结果而非错误，Recall 原样透传。
	hits, err := s.vs.Search(vecs[0], topK)
	if err != nil {
		return nil, fmt.Errorf("memory: search: %w", err)
	}

	// 只暴露文本：工具结果要拼进 prompt 给模型看，
	// Score 对模型没有信息量，给了反而稀释注意力。
	facts := make([]string, 0, len(hits))
	for _, h := range hits {
		facts = append(facts, h.Doc.Text)
	}
	return facts, nil
}
```

```go
func (t MemorySave) Execute(args string) (string, error) {
	var p struct {
		Fact string `json:"fact"`
	}
	// args 是模型生成的不可信输入：畸形 JSON 返回 error 喂回模型自我纠正。
	// 注意 tools.decodeArgs 未导出，跨包用不了，这里自己 Unmarshal。
	if err := json.Unmarshal([]byte(args), &p); err != nil {
		return "", fmt.Errorf("memory_save: invalid arguments %q: %w", args, err)
	}
	if err := t.Store.Remember(p.Fact); err != nil {
		return "", err
	}
	return "已记住：" + strings.TrimSpace(p.Fact), nil
}

func (t MemoryRecall) Execute(args string) (string, error) {
	var p struct {
		Query string `json:"query"`
		TopK  int    `json:"top_k"`
	}
	if err := json.Unmarshal([]byte(args), &p); err != nil {
		return "", fmt.Errorf("memory_recall: invalid arguments %q: %w", args, err)
	}

	facts, err := t.Store.Recall(p.Query, p.TopK)
	if err != nil {
		return "", err
	}
	// 明确的否定结果比空字符串更利于模型推理：
	// 空串会让模型分不清"没查到"和"工具坏了"。
	if len(facts) == 0 {
		return "没有找到相关记忆。", nil
	}

	var b strings.Builder
	b.WriteString("检索到以下相关记忆：\n")
	for i, f := range facts {
		fmt.Fprintf(&b, "%d. %s\n", i+1, f)
	}
	return b.String(), nil
}
```

### `cmd/agent/main.go` 的注册片段（由你手动加入，不属于 memory 包）

```go
embClient := embed.NewClient(os.Getenv("SILICONFLOW_API_KEY"))
memVS := vectorstore.NewStore()
_ = memVS.Load("memory.json") // 启动时恢复上次会话的记忆；文件不存在则忽略错误从空库开始
memStore := memory.NewStore(memVS, embClient, "memory.json")
registry.Register(memory.MemorySave{Store: memStore})
registry.Register(memory.MemoryRecall{Store: memStore})
```

### `internal/memory/memory_test.go`（新建，假 Embedder，无网络依赖）

```go
package memory

import (
	"path/filepath"
	"strings"
	"testing"

	"mini-agent/internal/vectorstore"
)

// fakeEmbedder 按文本查表返回预定义向量，未知文本返回 defaultVec。
// 用 2 维小向量即可表达"语义远近"：同方向 = 语义相近。
type fakeEmbedder struct {
	vecs       map[string][]float32
	defaultVec []float32
}

func (f fakeEmbedder) Embed(texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v, ok := f.vecs[t]
		if !ok {
			v = f.defaultVec
		}
		out[i] = v
	}
	return out, nil
}

// newTestStore 建一个记忆库：两条事实，"不吃辣"与"猫叫年糕"方向正交。
// 查询"饮食偏好"的向量靠近"不吃辣"，语义命中应返回它。
func newTestStore(t *testing.T, path string) *Store {
	t.Helper()
	emb := fakeEmbedder{
		vecs: map[string][]float32{
			"用户不吃辣":   {1, 0},
			"用户的猫叫年糕": {0, 1},
			"饮食偏好":    {1, 0.1},
		},
		defaultVec: []float32{0.5, 0.5},
	}
	return NewStore(vectorstore.NewStore(), emb, path)
}

// TestRememberThenRecall_SemanticHit 验证核心链路：Remember 后 Recall 按语义命中。
func TestRememberThenRecall_SemanticHit(t *testing.T) {
	s := newTestStore(t, filepath.Join(t.TempDir(), "memory.json"))

	if err := s.Remember("用户不吃辣"); err != nil {
		t.Fatalf("Remember: %v", err)
	}
	if err := s.Remember("用户的猫叫年糕"); err != nil {
		t.Fatalf("Remember: %v", err)
	}

	facts, err := s.Recall("饮食偏好", 1)
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(facts) != 1 || facts[0] != "用户不吃辣" {
		t.Errorf("Recall = %v, want [用户不吃辣]（语义命中失败）", facts)
	}
}

// TestRemember_RejectsEmpty 空事实必须报错，不进库。
func TestRemember_RejectsEmpty(t *testing.T) {
	s := newTestStore(t, filepath.Join(t.TempDir(), "memory.json"))
	if err := s.Remember("   "); err == nil {
		t.Error("blank fact: want error, got nil")
	}
}

// TestRecall_EmptyStore 空库 Recall 返回空结果，不算错误。
func TestRecall_EmptyStore(t *testing.T) {
	s := newTestStore(t, filepath.Join(t.TempDir(), "memory.json"))
	facts, err := s.Recall("随便查点什么", 3)
	if err != nil {
		t.Fatalf("Recall on empty store: %v", err)
	}
	if len(facts) != 0 {
		t.Errorf("got %d facts, want 0", len(facts))
	}
}

// TestRecall_TopKDefault topK<=0 时兜底为默认值而不是报错。
func TestRecall_TopKDefault(t *testing.T) {
	s := newTestStore(t, filepath.Join(t.TempDir(), "memory.json"))
	if err := s.Remember("用户不吃辣"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Recall("饮食偏好", 0); err != nil {
		t.Errorf("topK=0: want no error (default kicks in), got %v", err)
	}
}

// TestPersistence_RoundTrip 持久化往返：Remember 落盘后，
// 换一个全新向量库 Load 回来，Recall 仍能命中——这是"跨会话"的核心。
func TestPersistence_RoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.json")

	s1 := newTestStore(t, path)
	if err := s1.Remember("用户不吃辣"); err != nil {
		t.Fatalf("Remember: %v", err)
	}

	// 模拟进程重启：全新的 vectorstore，从磁盘 Load 恢复。
	vs2 := vectorstore.NewStore()
	if err := vs2.Load(path); err != nil {
		t.Fatalf("Load: %v", err)
	}
	s2 := NewStore(vs2, s1.emb, path)

	facts, err := s2.Recall("饮食偏好", 1)
	if err != nil {
		t.Fatalf("Recall after reload: %v", err)
	}
	if len(facts) != 1 || facts[0] != "用户不吃辣" {
		t.Errorf("Recall after reload = %v, want [用户不吃辣]", facts)
	}
}

// TestMemorySaveTool_Execute 工具层：合法参数写入成功并返回确认文本。
func TestMemorySaveTool_Execute(t *testing.T) {
	s := newTestStore(t, filepath.Join(t.TempDir(), "memory.json"))
	tool := MemorySave{Store: s}

	out, err := tool.Execute(`{"fact":"用户不吃辣"}`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "用户不吃辣") {
		t.Errorf("output %q does not confirm the fact", out)
	}

	facts, err := s.Recall("饮食偏好", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 1 || facts[0] != "用户不吃辣" {
		t.Errorf("fact not in store after tool Execute: %v", facts)
	}
}

// TestMemoryRecallTool_Execute 工具层：有记忆返回编号列表，空库返回明确否定。
func TestMemoryRecallTool_Execute(t *testing.T) {
	s := newTestStore(t, filepath.Join(t.TempDir(), "memory.json"))
	tool := MemoryRecall{Store: s}

	out, err := tool.Execute(`{"query":"饮食偏好"}`)
	if err != nil {
		t.Fatalf("Execute on empty: %v", err)
	}
	if !strings.Contains(out, "没有找到相关记忆") {
		t.Errorf("empty store output %q, want 明确的否定结果", out)
	}

	if err := s.Remember("用户不吃辣"); err != nil {
		t.Fatal(err)
	}
	out, err = tool.Execute(`{"query":"饮食偏好","top_k":1}`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "用户不吃辣") {
		t.Errorf("output %q missing the fact", out)
	}
}

// TestTools_BadJSON 畸形 JSON 必须返回 error（喂回模型自我纠正），不能 panic。
func TestTools_BadJSON(t *testing.T) {
	s := newTestStore(t, filepath.Join(t.TempDir(), "memory.json"))
	if _, err := (MemorySave{Store: s}).Execute(`{not json`); err == nil {
		t.Error("memory_save bad JSON: want error, got nil")
	}
	if _, err := (MemoryRecall{Store: s}).Execute(`{not json`); err == nil {
		t.Error("memory_recall bad JSON: want error, got nil")
	}
}
```

## 二、关键设计点

1. **本包是"薄封装"，设计含量在取舍不在算法**：Remember = embed + Add + Save，Recall = embed + Search + 提取 Text，没有一行新算法。这正印证阶段文档的考点"memory 与 RAG 技术栈相同，区别在写入路径与召回时机"。面试被问"长期记忆怎么实现"时，先讲这个等价关系，再讲"写什么/何时回忆"的策略选择，比背 MemGPT 名词更显真做过。

2. **写入即落盘的取舍**：每次 Remember 都全量 Save。记忆条数是几十上百量级，全量写毫秒级，换来的是"任何时候进程死掉都不丢记忆"的简单保证。**易错处**：为了"性能"把 Save 攒到退出时做——agent 是长进程，退出路径（Ctrl+C、panic）常常走不到清理逻辑，记忆悄悄丢失。真要优化也是增量持久化（append-only 日志），不是延迟写。

3. **topK 的宽窄分层是有意的**：`vectorstore.Search` 对 topK<=0 报错（面向代码的底层库，传 0 几乎一定是 bug），`memory.Recall` 对 topK<=0 兜底为 3（面向模型的边界层，模型不传 top_k 是常态）。同一个参数在两层语义相反，原因是调用方不同：对不可信输入宽容，对自己人严格。**易错处**：在 Recall 里直接透传 topK 给 Search，模型不传 top_k 时工具永远报错，ReAct 循环里模型反复重试同一个失败调用。

4. **工具输出面向模型而非面向人**：Recall 只返回 []string 不返回 Score；空结果返回"没有找到相关记忆。"而不是空串或 error。判断标准始终是"模型拿到这段文本能不能正确继续推理"——空串会让模型分不清"没记忆"和"工具故障"，error 会让模型以为可以重试（而重试空库永远还是空）。

5. **ID 用时间戳的限度**：`time.Now().UnixNano()` 在单进程内不会重复，够本练习用。它的局限是分布式/多进程写入时不保证唯一——生产上会换 UUID 或雪花 ID。基础版另一个刻意没做的点是**去重与遗忘**：用户两次说"我不吃辣"会入库两条（检索时占掉两个 top-k 名额）；用户改主意"我现在吃辣了"旧记忆也不会失效。这两个问题的**完整实现**（Remember 精确文本去重 + Forget 语义删除）见本文"三、进阶实现"一节——其中"为什么去重只做精确匹配、遗忘为什么要设高阈值"正是生产 memory 系统核心难点（记忆更新/冲突解决/遗忘策略）的最小缩影，面试主动展开是加分项。

6. **错误一律向上返回，不在工具层兜掉**：Remember/Recall 的 error 从 Execute 原样返回，ReAct 循环会把错误文本作为 tool 消息喂回模型，模型据此自我纠正（阶段一已验证的机制）。**易错处**：在 Execute 里 `log.Println(err); return "", nil`——错误被日志吃掉，模型看到的却是"工具成功但返回空"，推理直接跑偏。

7. **测试用查表式假 Embedder**：用 2 维正交向量表达"语义远近"（`{1,0}` vs `{0,1}`），不打真实 API。这样语义命中、持久化往返、空库三类行为都能在 CI 里确定性复现。持久化测试的关键手法是**换一个全新的 vectorstore.NewStore() 再 Load**——模拟进程重启，而不是复用同一个内存库（那测不出落盘是否真的发生）。

## 三、进阶实现：去重与遗忘

> 本节是"关键设计点"第 5 条的完整落地，2026-08-06 回补。代码已在 `/tmp` 项目副本中
> 实测验证（基础版 8 个测试不受影响 + 新增 5 个进阶测试全绿），不进项目代码树——
> 项目里请自己实现。

### 取舍：两个"保守优先"的决定

**① 去重只做精确文本匹配，不做语义去重。** 直觉上"语义相似的事实应该合并"，但这恰恰
危险：embedding 把"用户不吃辣"和"用户现在吃辣了"判定为高度相似（共享几乎全部词汇），
而它们的含义**相反**——语义去重会把这类"事实更新"误判为重复丢弃，阈值调不好甚至会
把旧事实删错。精确文本去重保守但安全：漏掉"我不吃辣"/"用户不吃辣"这种措辞不同的重复，
代价只是多占一个 top-k 名额；误删真实信息的代价是记忆系统失去可信度。生产的记忆
更新/冲突解决（如 MemGPT 的记忆编辑、Zep 的事实时效管理）是远超本练习的课题，
这里用最小实现把"为什么不能简单语义判重"讲清楚。

**② 遗忘（Forget）必须设高相似度阈值。** 检索的 top-1 **永远存在**——哪怕库里的记忆
全与 query 无关，也总有一条"最相似的"。不设阈值的话，`Forget("火星气候")` 会把
最无辜的一条记忆删掉。删除是不可逆的破坏操作，所以阈值定在 0.9（接近"就是这条"），
拿不准就不删，返回 0 让调用方如实告知"没找到"。这与 kb_search 的 minScore 是同一个
思想（top-k 只保证相对最近），但阈值高得多——检索错放噪声只是稀释 prompt，
删除错删记忆是丢数据。

### vectorstore 依赖：FindByMetadata + Delete

两个方法在**练习 4 参考答案的"三、进阶实现"一节**已给出完整代码与测试
（`internal/vectorstore/delete.go` + `delete_test.go`），本节直接复用，不再重复——
若你只在做练习 5，请先从练习 4 答案搬运这两个方法。Remember 去重用
`FindByMetadata("kind", "memory")` 扫描已有记忆，Forget 用 `Delete(id)` 删除命中的记忆。

### Remember 升级：写入前精确去重（`internal/memory/memory.go`，替换基础版 Remember）

行为变化只有一处：完全相同的事实重复 Remember 时跳过（返回 nil，不重复入库、
不调 embedding）。空事实校验、落盘纪律与基础版一致（基础版 8 个测试原样通过）。

```go
// Remember 把一条事实写入长期记忆并立即持久化：embed → 入库 → Save 落盘。
//
// 进阶：写入前做精确文本去重——完全相同的事实已存在则直接跳过，
// 不重复入库、也不再调 embedding（见设计点：为什么只做精确去重）。
func (s *Store) Remember(fact string) error {
	// 空事实没有语义，在最早的层面拦下——embed.Client 也会拒绝空串，
	// 但让坏输入多走一层没有意义。
	fact = strings.TrimSpace(fact)
	if fact == "" {
		return fmt.Errorf("memory: empty fact")
	}

	// 精确文本去重（进阶）：逐条比对已有记忆的 Text，完全相同则跳过。
	// 刻意不做语义级去重（相似度阈值判重）："用户不吃辣"和"用户现在吃辣了"
	// 语义高度相似但含义相反，语义去重会把这类"更新"误判为重复而丢弃，
	// 或者更糟——把旧事实删掉。宁可选保守的精确匹配（漏掉措辞不同的重复，
	// 代价只是多占一个 top-k 名额），也不冒误删真实信息的风险。
	for _, d := range s.vs.FindByMetadata("kind", "memory") {
		if d.Text == fact {
			return nil
		}
	}

	// embed 是批量接口，单条也要包成单元素切片。
	vecs, err := s.emb.Embed([]string{fact})
	if err != nil {
		return fmt.Errorf("memory: embed fact: %w", err)
	}

	doc := vectorstore.Document{
		// ID 只需唯一：纳秒时间戳足够（单进程内不会重复）。
		ID:       fmt.Sprintf("mem-%d", time.Now().UnixNano()),
		Text:     fact,
		Vector:   vecs[0],
		Metadata: map[string]string{"kind": "memory"},
	}
	if err := s.vs.Add(doc); err != nil {
		return fmt.Errorf("memory: add fact: %w", err)
	}

	// 长期记忆的价值就在"跨会话"：写内存不落盘等于没记。
	// 每次写入即全量 Save，数据量小（几十上百条）时完全够用；
	// 保存失败必须返回错误，不能吞——否则用户以为记住了，重启后丢失。
	if err := s.vs.Save(s.path); err != nil {
		return fmt.Errorf("memory: persist: %w", err)
	}
	return nil
}
```

### Forget：按语义找到最相似的一条并删除（同文件新增）

```go
// forgetMinScore 是"允许遗忘"的最低相似度（进阶）。
//
// 为什么遗忘要设高阈值：检索是"找最相似的"，哪怕库里的记忆全与 query
// 无关，top-1 也永远存在——不设阈值的话，Forget("火星气候") 会把
// 最无辜的一条记忆删掉。删除是不可逆的破坏操作，所以阈值定得很高
// （0.9，接近"就是这条"），拿不准就不删，返回 0 让调用方如实告知。
const forgetMinScore = 0.9

// Forget 按语义找到与 query 最相似的一条记忆并删除（进阶：遗忘）。
// 返回删除的条数（0 或 1）：没有足够相似的记忆时返回 0 + nil，
// 一条都不动——"没找到"不是错误，但"删错了"是事故。
func (s *Store) Forget(query string) (int, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return 0, fmt.Errorf("memory: empty query")
	}
	// 空库短路：没什么好忘的，也别浪费一次 embedding 调用。
	if s.vs.Len() == 0 {
		return 0, nil
	}

	vecs, err := s.emb.Embed([]string{query})
	if err != nil {
		return 0, fmt.Errorf("memory: embed query: %w", err)
	}
	hits, err := s.vs.Search(vecs[0], 1)
	if err != nil {
		return 0, fmt.Errorf("memory: search: %w", err)
	}
	if len(hits) == 0 || hits[0].Score < forgetMinScore {
		return 0, nil
	}

	s.vs.Delete(hits[0].Doc.ID)
	// 与 Remember 同样的纪律：改动立即落盘，保存失败要上报。
	if err := s.vs.Save(s.path); err != nil {
		return 0, fmt.Errorf("memory: persist: %w", err)
	}
	return 1, nil
}
```

import 与基础版相同（`encoding/json`、`fmt`、`strings`、`time`、`tools`、`vectorstore`），无新增依赖。

两个刻意的简化（面试可主动讲）：Forget 只删 top-1 一条（用户说"忘掉我不吃辣"指向的
就是一条事实）；且假设记忆库专用（若将来与知识库混存一个 vectorstore，Search 的 top-1
可能命中知识块而非记忆，需要先用 Metadata["kind"] 过滤候选——本实现没做这层，
因为 main.go 里记忆与知识库本来就是两个独立的 Store）。

### 进阶测试（`internal/memory/forget_test.go`，新建，与 memory_test.go 同包）

```go
package memory

import (
	"path/filepath"
	"testing"

	"mini-agent/internal/vectorstore"
)

// countingEmbedder 在查表假 Embedder 之上记录 embed 的文本条数：
// 去重的收益之一是"重复事实一次 embedding 都不调"，必须能断言。
type countingEmbedder struct {
	vecs       map[string][]float32
	defaultVec []float32
	calls      int
}

func (c *countingEmbedder) Embed(texts []string) ([][]float32, error) {
	c.calls += len(texts)
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v, ok := c.vecs[t]
		if !ok {
			v = c.defaultVec
		}
		out[i] = v
	}
	return out, nil
}

// newForgetTestStore 建一个带计数 embedder 的记忆库：
// "用户不吃辣" 与 "用户的猫叫年糕" 正交；"不吃辣" 与前者同向（得分 1.0）；
// 未知文本走 defaultVec {0.5,0.5}，与两者的余弦都约 0.707 < forgetMinScore。
func newForgetTestStore(t *testing.T, path string) (*Store, *countingEmbedder) {
	t.Helper()
	emb := &countingEmbedder{
		vecs: map[string][]float32{
			"用户不吃辣":   {1, 0},
			"用户的猫叫年糕": {0, 1},
			"饮食偏好":    {1, 0.1},
			"不吃辣":      {1, 0}, // 与事实完全同向 → Forget 得分 1.0
		},
		defaultVec: []float32{0.5, 0.5},
	}
	return NewStore(vectorstore.NewStore(), emb, path), emb
}

// TestRemember_ExactDuplicateSkipped 验证精确文本去重：
// 同一条事实 Remember 两次，库里只有一条，且第二次连 embedding 都没调。
func TestRemember_ExactDuplicateSkipped(t *testing.T) {
	s, emb := newForgetTestStore(t, filepath.Join(t.TempDir(), "memory.json"))

	if err := s.Remember("用户不吃辣"); err != nil {
		t.Fatal(err)
	}
	callsAfterFirst := emb.calls
	if err := s.Remember("用户不吃辣"); err != nil {
		t.Fatal(err)
	}

	if got := len(s.vs.FindByMetadata("kind", "memory")); got != 1 {
		t.Errorf("重复 Remember 后库里有 %d 条, want 1", got)
	}
	if emb.calls != callsAfterFirst {
		t.Errorf("重复 Remember 多调了 %d 次 embedding，去重短路未生效", emb.calls-callsAfterFirst)
	}
}

// TestRemember_SimilarButDifferentKept 验证去重只做精确匹配：
// "用户现在吃辣了"与"用户不吃辣"语义相近但文本不同，必须两条都保留——
// 这正是参考答案强调"语义去重危险"的原因：这类"更新"不能被误判为重复。
func TestRemember_SimilarButDifferentKept(t *testing.T) {
	s, _ := newForgetTestStore(t, filepath.Join(t.TempDir(), "memory.json"))

	if err := s.Remember("用户不吃辣"); err != nil {
		t.Fatal(err)
	}
	if err := s.Remember("用户现在吃辣了"); err != nil {
		t.Fatal(err)
	}
	if got := len(s.vs.FindByMetadata("kind", "memory")); got != 2 {
		t.Errorf("语义相近但文本不同的两条事实只存了 %d 条, want 2", got)
	}
}

// TestForget_RemovesMostSimilar 验证遗忘主链路：
// 按语义找到最相似的一条删除，返回 1；之后 Recall 查不到它；
// 其他记忆不受影响；删除已落盘（重启后依然不存在）。
func TestForget_RemovesMostSimilar(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.json")
	s, _ := newForgetTestStore(t, path)

	if err := s.Remember("用户不吃辣"); err != nil {
		t.Fatal(err)
	}
	if err := s.Remember("用户的猫叫年糕"); err != nil {
		t.Fatal(err)
	}

	// "不吃辣" 与 "用户不吃辣" 同向（得分 1.0 >= 阈值），应被删掉。
	n, err := s.Forget("不吃辣")
	if err != nil {
		t.Fatalf("Forget: %v", err)
	}
	if n != 1 {
		t.Fatalf("Forget 返回 %d, want 1", n)
	}

	// Recall 结果里不能再有被遗忘的事实；另一条记忆必须还在。
	facts, err := s.Recall("饮食偏好", 3)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range facts {
		if f == "用户不吃辣" {
			t.Errorf("被遗忘的事实仍能被 Recall 命中：%v", facts)
		}
	}
	if got := len(s.vs.FindByMetadata("kind", "memory")); got != 1 {
		t.Errorf("Forget 后库里剩 %d 条, want 1（误删了其他记忆？）", got)
	}

	// 模拟重启：从磁盘恢复后，被遗忘的事实依然不存在（删除已落盘）。
	vs2 := vectorstore.NewStore()
	if err := vs2.Load(path); err != nil {
		t.Fatalf("Load: %v", err)
	}
	s2 := NewStore(vs2, s.emb, path)
	facts, err = s2.Recall("饮食偏好", 3)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range facts {
		if f == "用户不吃辣" {
			t.Errorf("重启后被遗忘的事实复活了（删除未落盘）：%v", facts)
		}
	}
}

// TestForget_NoSimilarMemoryReturnsZero 验证安全闸：
// query 与库中所有记忆都不足够相似（得分 < 阈值）时，返回 0 且一条都不删。
func TestForget_NoSimilarMemoryReturnsZero(t *testing.T) {
	s, _ := newForgetTestStore(t, filepath.Join(t.TempDir(), "memory.json"))

	if err := s.Remember("用户不吃辣"); err != nil {
		t.Fatal(err)
	}
	// "火星气候" 走 defaultVec，与 "用户不吃辣" 的余弦约 0.707 < 0.9。
	n, err := s.Forget("火星气候")
	if err != nil {
		t.Fatalf("Forget: %v", err)
	}
	if n != 0 {
		t.Errorf("Forget 返回 %d, want 0（不相似的记忆不应被删）", n)
	}
	if got := len(s.vs.FindByMetadata("kind", "memory")); got != 1 {
		t.Errorf("不相似的 Forget 删除了记忆：剩 %d 条, want 1", got)
	}
}

// TestForget_EdgeCases 空 query 报错；空库返回 0 不报错、不调 embedding。
func TestForget_EdgeCases(t *testing.T) {
	s, emb := newForgetTestStore(t, filepath.Join(t.TempDir(), "memory.json"))

	if _, err := s.Forget("   "); err == nil {
		t.Error("blank query: want error, got nil")
	}
	n, err := s.Forget("随便忘点什么")
	if err != nil || n != 0 {
		t.Errorf("空库 Forget = (%d, %v), want (0, nil)", n, err)
	}
	if emb.calls != 0 {
		t.Errorf("空库 Forget 调了 %d 次 embedding（应短路）", emb.calls)
	}
}
```

### 易错处

1. **"找到最相似的"不等于"找到了"**：这是本节与 kb_search 低分过滤共用的核心认知。
   Forget 没有阈值 = 每次调用必删一条，测试 `TestForget_NoSimilarMemoryReturnsZero`
   用约 0.707 的得分（低于 0.9）专查这个安全闸。
2. **去重扫描要在 embed 之前**：先比对文本再调 embedding，重复事实一次 API 调用都不花；
   顺序反过来（先 embed 再判重）功能也对，但去重的成本收益就没了——测试用
   countingEmbedder 断言调用次数，防的就是这个顺序漂移。
3. **删除后必须落盘**：Forget 改完内存不 Save，进程重启后被遗忘的记忆"复活"——
   测试 `TestForget_RemovesMostSimilar` 用"换全新 vectorstore 重新 Load"验证这一点，
   与基础版持久化测试是同一个手法。
4. **0.9 阈值是针对 bge-m3 量级拍的经验值**：真实部署应按自己的 embedding 模型用
   标注数据校准；语义近似但措辞不同的遗忘请求（"忘掉我的饮食禁忌"）可能达不到 0.9，
   这是保守策略的有意代价——漏删可以让用户说得更具体后重试，误删无法挽回。

### 验证记录（2026-08-06 回补）

在 `/tmp/verify-ex45`（`cp -r mini-agent` 的副本，不影响在制代码）中应用
练习 2/5 基础答案 + 练习 4 进阶的 vectorstore 删除能力 + 本节进阶代码 + 上述测试：

```
cd /tmp/verify-ex45
go vet ./...                          # 通过
go test ./internal/memory/ -v         # 13 个测试全 PASS（8 基础 + 5 去重/遗忘进阶）
go test ./internal/vectorstore/ -v    # 3 个新增测试全 PASS（FindByMetadata/Delete/dim 重置）
```

验证后副本已删除，项目代码未做任何改动（mini-agent/ 下 git status 与回补前一致）。

## 四、对照清单

完成后逐条自评（不要求与答案一字不差，覆盖条目即可）：

- [x] `Remember` 链路完整：空事实校验 → embed → Add（唯一 ID）→ Save 落盘，每步错误都包装后上抛
- [x] `Recall` 链路完整：topK<=0 有默认值兜底 → embed → Search → 只返回文本列表；空库返回空不报错
- [x] 两个工具的 Execute：畸形 JSON 返回 error 不 panic；`memory_recall` 空结果返回明确的否定文本
- [x] 持久化往返测试用 `t.TempDir()` + **全新的 Store 实例** Load，能命中重启前 Remember 的事实
- [x] main.go 注册成功，端到端走通："记住我不吃辣" → 重启 → "我喜欢吃什么"模型先调 memory_recall 再回答
- [x] `go vet ./...` 和 `go test ./internal/memory/` 全绿
- [x] 能口头回答：长期 memory 与 RAG 的异同？为什么选"模型主动调工具"而不是"每轮自动检索"？记忆去重/遗忘为什么难？
- [ ] （进阶）Remember 精确文本去重：重复事实只存一条、第二次不调 embedding；语义相近但文本不同的事实（"不吃辣"/"现在吃辣了"）两条都保留
- [ ] （进阶）`Forget(query)`：top-1 得分达高阈值（0.9）才删并立即落盘；不相似返回 0 一条不动；空库短路、空 query 报错
- [ ] （进阶）能讲清取舍：为什么去重只做精确匹配（语义去重会把"事实更新"误判为重复）？为什么遗忘阈值比检索阈值高得多（top-1 永远存在，误删不可逆）？
