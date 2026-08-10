# 练习 3 参考答案：chunking 文档切分

> 对应 TODO：`mini-agent/internal/rag/chunk.go` 的 `TODO(练习3)`。
> **完成练习并自评后再看本文档。**
> 本文档代码已于 2026-08-06 实际粘贴进项目验证：`cd mini-agent && go vet ./... && go test ./internal/rag/ -v`（7 个测试）全部通过；验证后项目代码已恢复为骨架版。
> 2026-08-06 进阶回补：新增"三、进阶实现"一节（句级二级拆分 + 词边界回退，对应原"关键设计点"第 6 条的加分项 ①②），代码与 6 个新增测试同样实际粘贴进项目验证（连同基础版共 13 个测试全绿），验证后项目已恢复为骨架版。

---

## 一、参考实现

### `internal/rag/chunk.go`（Chunk 的实现及三个辅助函数；骨架其余部分不变）

import 需要加上 `strings`：

```go
import "strings"
```

```go
// Chunk 把一篇文档切成若干不超过 opts.MaxChars 字符（rune）的块。
//
// 策略是"结构优先，窗口兜底"：
//  1. 先按空行切段落，再贪心地把整段打包进块，段落不被拆散；
//  2. 单段自身超过 MaxChars 时，才对该段做固定窗口硬切（带重叠）。
//
// 全程按 rune 而非 byte 计量——中文一个字符占 3 个 byte，
// 按 byte 切会把汉字劈成乱码。
func Chunk(text string, opts ChunkOptions) []string {
 opts = normalizeChunkOptions(opts)

 var chunks []string
 var cur []string // 当前块已打包的段落
 curLen := 0      // 当前块的 rune 数（含段落间 "\n\n" 分隔符）

 // flush 封存当前块。闭包捕获 cur/curLen/chunks，注意它同时重置前两者。
 flush := func() {
  if len(cur) == 0 {
   return
  }
  chunks = append(chunks, strings.Join(cur, "\n\n"))
  cur = nil
  curLen = 0
 }

 for _, para := range splitParagraphs(text) {
  paraLen := len([]rune(para))
  if paraLen > opts.MaxChars {
   // 超长段落：先封存当前块（保持段落顺序），再对该段硬切。
   flush()
   chunks = append(chunks, hardCut(para, opts)...)
   continue
  }
  addLen := paraLen
  if len(cur) > 0 {
   addLen += 2 // 段落间分隔符 "\n\n" 也占块内额度
  }
  if curLen+addLen > opts.MaxChars {
   flush() // 当前块放不下，封存后开新块
  }
  if len(cur) == 0 {
   curLen = paraLen
  } else {
   curLen += 2 + paraLen
  }
  cur = append(cur, para)
 }
 flush()

 return chunks
}

// normalizeChunkOptions 防御异常参数，保证硬切循环一定收敛：
// MaxChars <= 0 用默认值兜底；OverlapChars 必须落在 [0, MaxChars-1]，
// 否则步长 MaxChars-OverlapChars 为 0 或负数，for 循环永不前进（死循环）。
func normalizeChunkOptions(opts ChunkOptions) ChunkOptions {
 if opts.MaxChars <= 0 {
  opts = DefaultChunkOptions()
 }
 if opts.OverlapChars < 0 {
  opts.OverlapChars = 0
 }
 if opts.OverlapChars >= opts.MaxChars {
  opts.OverlapChars = opts.MaxChars - 1
 }
 return opts
}

// splitParagraphs 按空行切段，TrimSpace 后丢弃空段——空段没有语义，
// 留着只会产出让 embedding 白跑一次的空块。
func splitParagraphs(text string) []string {
 var paras []string
 for _, p := range strings.Split(text, "\n\n") {
  if p = strings.TrimSpace(p); p != "" {
   paras = append(paras, p)
  }
 }
 return paras
}

// hardCut 对超长段落做固定窗口切分：每块最多 MaxChars 个 rune，
// 相邻块重叠 OverlapChars 个 rune（步长 = MaxChars - OverlapChars）。
// 参数已经过 normalizeChunkOptions 钳制，step 必然 >= 1。
func hardCut(para string, opts ChunkOptions) []string {
 runes := []rune(para)
 step := opts.MaxChars - opts.OverlapChars
 var out []string
 for start := 0; start < len(runes); start += step {
  end := start + opts.MaxChars
  if end > len(runes) {
   end = len(runes)
  }
  out = append(out, string(runes[start:end]))
  if end == len(runes) {
   break // 末尾块通常不足 MaxChars，与上一块的重叠会超过 OverlapChars，属正常
  }
 }
 return out
}
```

### `internal/rag/chunk_test.go`（新建，纯文本构造，无外部依赖）

```go
package rag

import (
 "strings"
 "testing"
 "time"
 "unicode/utf8"
)

// runeLen 是测试用的简写：本练习的核心纪律就是"按 rune 不按 byte"。
func runeLen(s string) int { return utf8.RuneCountInString(s) }

// TestChunk_PacksParagraphs 验证结构优先：多段能装进一个块时原样打包，
// 段落不被拆散，分隔符保留。
func TestChunk_PacksParagraphs(t *testing.T) {
 paras := []string{"第一段：甲乙丙丁", "第二段：戊己庚辛", "第三段：壬癸"}
 text := strings.Join(paras, "\n\n")

 chunks := Chunk(text, ChunkOptions{MaxChars: 100, OverlapChars: 10})
 if len(chunks) != 1 {
  t.Fatalf("len(chunks) = %d, want 1（三段应打包进同一块）", len(chunks))
 }
 if chunks[0] != text {
  t.Errorf("chunk 内容被改动：got %q, want %q", chunks[0], text)
 }
}

// TestChunk_PackingRespectsMaxChars 验证贪心打包的上限纪律：
// 两个 60 rune 的段落装不进 100 的块（60+2+60=122），必须分成两块，
// 且段落保持完整、每块都不超限。
func TestChunk_PackingRespectsMaxChars(t *testing.T) {
 p1 := strings.Repeat("甲", 60)
 p2 := strings.Repeat("乙", 60)
 text := p1 + "\n\n" + p2

 chunks := Chunk(text, ChunkOptions{MaxChars: 100, OverlapChars: 10})
 if len(chunks) != 2 {
  t.Fatalf("len(chunks) = %d, want 2", len(chunks))
 }
 if chunks[0] != p1 || chunks[1] != p2 {
  t.Errorf("段落被拆散或改序：got %q", chunks)
 }
 for i, c := range chunks {
  if runeLen(c) > 100 {
   t.Errorf("chunks[%d] = %d rune，超过 MaxChars=100", i, runeLen(c))
  }
 }
}

// TestChunk_HardCutWithOverlap 验证超长段硬切：
// 250 rune 单段，MaxChars=100、Overlap=30 → 步长 70，
// 起点 0/70/140/210，共 4 块；相邻块恰好重叠 30 个 rune；
// 去掉重叠拼接后必须还原原文（内容全覆盖）。
func TestChunk_HardCutWithOverlap(t *testing.T) {
 para := strings.Repeat("a", 250)
 opts := ChunkOptions{MaxChars: 100, OverlapChars: 30}

 chunks := Chunk(para, opts)
 if len(chunks) != 4 {
  t.Fatalf("len(chunks) = %d, want 4", len(chunks))
 }
 for i, c := range chunks {
  if runeLen(c) > opts.MaxChars {
   t.Errorf("chunks[%d] = %d rune，超过 MaxChars=%d", i, runeLen(c), opts.MaxChars)
  }
 }
 // 相邻块：前块末尾 OverlapChars 个 rune == 后块开头 OverlapChars 个 rune。
 for i := 0; i+1 < len(chunks); i++ {
  cur, next := []rune(chunks[i]), []rune(chunks[i+1])
  ov := opts.OverlapChars
  if string(cur[len(cur)-ov:]) != string(next[:ov]) {
   t.Errorf("chunks[%d] 与 chunks[%d] 之间重叠不正确", i, i+1)
  }
 }
 // 覆盖校验：首块 + 后续块各去掉前 OverlapChars 个重叠 rune，拼回应等于原文。
 var sb strings.Builder
 sb.WriteString(chunks[0])
 for _, c := range chunks[1:] {
  sb.WriteString(string([]rune(c)[opts.OverlapChars:]))
 }
 if sb.String() != para {
  t.Errorf("去重叠拼接后 = %d rune，与原文不一致（内容未全覆盖）", runeLen(sb.String()))
 }
}

// TestChunk_ChineseNoMojibake 验证最核心的坑：中文硬切不产生乱码。
// "人工智能" 每个汉字 3 byte，若实现按 byte 切，这里必挂。
func TestChunk_ChineseNoMojibake(t *testing.T) {
 para := strings.Repeat("人工智能", 40) // 160 rune = 480 byte
 opts := ChunkOptions{MaxChars: 50, OverlapChars: 10}

 chunks := Chunk(para, opts)
 if len(chunks) == 0 {
  t.Fatal("no chunks")
 }
 for i, c := range chunks {
  if !utf8.ValidString(c) {
   t.Fatalf("chunks[%d] 不是合法 UTF-8（按 byte 切产生了乱码）", i)
  }
  if runeLen(c) > opts.MaxChars {
   t.Errorf("chunks[%d] = %d rune，超过 MaxChars=%d", i, runeLen(c), opts.MaxChars)
  }
 }
 // 覆盖校验：拼接去重叠后还原 160 个汉字。
 var sb strings.Builder
 sb.WriteString(chunks[0])
 for _, c := range chunks[1:] {
  r := []rune(c)
  // 末尾块与上一块的重叠可能超过 OverlapChars，按实际起点对齐：
  // 这里步长 = 50-10 = 40，每块贡献 40 个新 rune，末尾块贡献其剩余部分。
  overlap := opts.OverlapChars
  if len(r) < overlap {
   overlap = len(r)
  }
  sb.WriteString(string(r[overlap:]))
 }
 if sb.String() != para {
  t.Errorf("中文文本去重叠拼接后与原文不一致：got %d rune, want %d", runeLen(sb.String()), runeLen(para))
 }
}

// TestChunk_EmptyAndBlankInput 验证空输入、全空白输入都返回 nil（不产空块）。
func TestChunk_EmptyAndBlankInput(t *testing.T) {
 if got := Chunk("", DefaultChunkOptions()); got != nil {
  t.Errorf(`Chunk("") = %v, want nil`, got)
 }
 if got := Chunk("  \n\n\t \n\n  ", DefaultChunkOptions()); got != nil {
  t.Errorf(`Chunk(全空白) = %v, want nil`, got)
 }
}

// TestChunk_OverlapClampedNoDeadLoop 验证防御：OverlapChars >= MaxChars 时
// 步长被钳制为 >= 1，硬切不死循环。用 goroutine + 超时探测死循环。
func TestChunk_OverlapClampedNoDeadLoop(t *testing.T) {
 para := strings.Repeat("x", 120)
 done := make(chan []string, 1)
 go func() {
  done <- Chunk(para, ChunkOptions{MaxChars: 50, OverlapChars: 50})
 }()
 select {
 case chunks := <-done:
  if len(chunks) == 0 {
   t.Error("overlap 被钳制后仍应产出块")
  }
  for i, c := range chunks {
   if runeLen(c) > 50 {
    t.Errorf("chunks[%d] = %d rune，超过 MaxChars=50", i, runeLen(c))
   }
  }
 case <-time.After(2 * time.Second):
  t.Fatal("2 秒未返回：OverlapChars >= MaxChars 导致死循环")
 }
}

// TestChunk_ZeroOptsUsesDefault 验证零值 opts 用默认值兜底，且正常切块。
func TestChunk_ZeroOptsUsesDefault(t *testing.T) {
 para := strings.Repeat("哈", 1000) // 1000 rune > 默认 400，必然触发硬切
 chunks := Chunk(para, ChunkOptions{})
 if len(chunks) < 2 {
  t.Fatalf("len(chunks) = %d, want >= 2（默认 MaxChars=400 应切多块）", len(chunks))
 }
 for i, c := range chunks {
  if runeLen(c) > 400 {
   t.Errorf("chunks[%d] = %d rune，超过默认 MaxChars=400", i, runeLen(c))
  }
 }
}
```

## 二、关键设计点

1. **结构优先、窗口兜底的两级策略**：先按空行切段落再贪心打包，保证大多数块是"语义完整"的（段落是天然的话题边界）；只有单段自身超限时才退化为固定窗口硬切。这是面试"chunk 怎么切"的标准答案骨架——纯固定窗口切会把段落拦腰截断，纯按结构切又可能产出超限巨块，两级都要。**易错处**：遇到超长段落时必须**先 flush 当前块**再硬切，否则超长段的硬切块会被插到已打包段落前面，顺序错乱。

2. **rune 而非 byte 是本练习的灵魂**：Go 的 `len(s)` 和 `s[i:j]` 都按 byte 计，中文 UTF-8 每字 3 byte，按 byte 切会把汉字劈成非法 UTF-8 序列（乱码）。所有长度判断和切片都必须先 `[]rune(s)` 转换。**易错处**：只把切片改成 rune、长度判断还残留 `len(para)`（byte 数），块会"虚胖"——中文场景下实际 rune 数只有 byte 数的 1/3，打包逻辑名义上没超 MaxChars 实际块数翻三倍。

3. **overlap 钳制防死循环**：硬切步长 = `MaxChars - OverlapChars`，若 `OverlapChars >= MaxChars` 步长为 0 或负，`for start := 0; ...; start += step` 永不前进。防御放在 `normalizeChunkOptions` 入口统一做，硬切函数内部就可以放心假设 step >= 1——**不变式在入口处建立，下游不用重复防御**。

4. **分隔符也占额度**：贪心打包时块内容量是"段落 + 段落间 `\n\n`"，漏算这 2 个 rune 会让块略微超限。数值上无伤大雅，但测试若断言"每块 <= MaxChars"就会挂——写实现前先想清楚"块的确切内容是什么"，长度账才能算平。

5. **覆盖性（coverage）是不变量**：除被 TrimSpace 掉的空白外，原文任何一段文字都应出现在至少一个块里。测试用"去重叠拼接还原原文"来验证，这比逐块比对更能抓住"漏切了一段"这类 bug。生产环境的对应物：入库后做文档级抽检，防止解析/切分环节悄悄丢内容（检索召回失败的隐蔽根因之一）。

6. **已知简化与进阶方向**：基础版有三个已知简化：① 按空行切段对"段内长句"无能为力，更精细的做法是句级二级拆分；② 硬切不看词边界，英文单词可能被劈开，生产实现会在窗口内回退到最近的空白/标点处下刀；③ 按字符而非 token 计数。其中 ①② 的**完整实现**见本文"三、进阶实现"一节（面试加分项，已验证可直接用）；③ 是开放讨论，无需实现（理由见该节末尾）。面试时知道简化在哪、并能讲清进阶方案怎么落地，比假装没有简化重要。

## 三、进阶实现（加分项落地）

> 本节是"关键设计点"第 6 条加分项 ①② 的完整实现，在基础版两级策略（段落打包 + 硬切兜底）
> 中间插入句级，并把硬切升级为边界感知。**独立于基础版的 `Chunk`，新函数命名为
> `ChunkRefined`**——基础版保持教学上的简单，进阶版按需选用，两者共存便于 A/B 对比。
> 已于 2026-08-06 粘贴进项目实测：`go vet ./...` 与 `go test ./internal/rag/ -v`
> （基础 7 个 + 进阶 6 个，共 13 个测试）全绿。

策略变成三级：**段落 → 句子 → 边界感知硬切**。装得下的段落仍整段打包（与基础版一致，
无回归）；只有超长段落才拆句，超长单句才硬切——每一级都是上一级的兜底，成本只对
"真的需要精细处理"的文本支付。

import 需要在基础版的 `strings` 之上再加 `unicode`：

```go
import (
 "strings"
 "unicode"
)
```

```go
// ChunkRefined 是 Chunk 的精细版：块边界尽量落在句子上，硬切不劈单词。
//
// 与 Chunk 的唯一差异在超长段落的处理：Chunk 直接硬切，ChunkRefined 先做
// 句级二级拆分（chunkSentences），单句仍超长才退化为边界感知硬切
//（hardCutBoundary）。段落打包路径与 Chunk 逐行一致——未触发超限时
// 两个函数输出完全相同（有测试保证）。
func ChunkRefined(text string, opts ChunkOptions) []string {
 opts = normalizeChunkOptions(opts)

 var chunks []string
 var cur []string
 curLen := 0

 flush := func() {
  if len(cur) == 0 {
   return
  }
  chunks = append(chunks, strings.Join(cur, "\n\n"))
  cur = nil
  curLen = 0
 }

 for _, para := range splitParagraphs(text) {
  if len([]rune(para)) > opts.MaxChars {
   // 超长段落：先封存当前块（保持顺序），再做句级二级拆分。
   flush()
   chunks = append(chunks, chunkSentences(para, opts)...)
   continue
  }
  paraLen := len([]rune(para))
  addLen := paraLen
  if len(cur) > 0 {
   addLen += 2
  }
  if curLen+addLen > opts.MaxChars {
   flush()
  }
  if len(cur) == 0 {
   curLen = paraLen
  } else {
   curLen += 2 + paraLen
  }
  cur = append(cur, para)
 }
 flush()

 return chunks
}

// chunkSentences 把超长段落按句拆分后贪心打包；单句仍超限才边界感知硬切。
//
// 与段落打包的两处差异：
//   - 句子之间用 "" 连接而非 "\n\n"——句末标点/换行已保留在句尾（splitSentences
//     的设计），再加分隔符反而篡改原文；
//   - 句级块之间不加重叠——句子是完整的语义单元，块边界落在句子上时重叠的收益
//     很小；重叠留给"句内被劈开"的硬切路径。
func chunkSentences(para string, opts ChunkOptions) []string {
 var out []string
 var cur []string
 curLen := 0

 flush := func() {
  if len(cur) == 0 {
   return
  }
  out = append(out, strings.Join(cur, ""))
  cur = nil
  curLen = 0
 }

 for _, sent := range splitSentences(para) {
  sentLen := len([]rune(sent))
  if sentLen > opts.MaxChars {
   flush()
   out = append(out, hardCutBoundary(sent, opts)...)
   continue
  }
  if curLen+sentLen > opts.MaxChars {
   flush()
  }
  cur = append(cur, sent)
  curLen += sentLen
 }
 flush()
 return out
}

// splitSentences 按句末标点（。！？!?）和换行拆句，分隔符保留在句尾。
//
// 易错处：句子**原样保留**（含前导空白），只丢弃全空白句。若像段落那样
// TrimSpace，英文 "Hello world. Foo." 拆句再拼接会变成 "Hello world.Foo."
// ——句间空格丢失，覆盖性不变量被破坏。原样保留保证 strings.Join(sents, "")
// 无损还原原文。
func splitSentences(para string) []string {
 var sents []string
 start := 0
 runes := []rune(para)
 for i, r := range runes {
  if isSentenceEnd(r) {
   sents = appendSent(sents, runes[start:i+1])
   start = i + 1
  }
 }
 if start < len(runes) {
  sents = appendSent(sents, runes[start:])
 }
 return sents
}

func appendSent(sents []string, r []rune) []string {
 if strings.TrimSpace(string(r)) == "" {
  return sents
 }
 return append(sents, string(r))
}

func isSentenceEnd(r rune) bool {
 switch r {
 case '。', '！', '？', '!', '?', '\n':
  return true
 }
 return false
}

// hardCutBoundary 边界感知的硬切：窗口右端向左回退到最近的空白/标点处下刀。
//
// 三个设计要点：
//  1. 回退有下限 floor = start + MaxChars/2——最多回退半个窗口，防止"上一个
//     边界刚好在窗口开头"时产出过小的块（极端情况下块退化成几个字符）；
//  2. 下限内找不到边界就放弃回退、原地硬切——长串无空白无标点的文本（乱码、
//     base64、URL）不能因此产不出块；
//  3. 下一块起点 next = cut - OverlapChars，且必须 next > start——重叠在
//     极端情况下（cut 距 start 比 OverlapChars 还近）可能把 next 拉回 start
//     之前造成死循环，钳制到 cut 兜底：宁可丢重叠，不可死循环。
func hardCutBoundary(s string, opts ChunkOptions) []string {
 runes := []rune(s)
 var out []string
 for start := 0; start < len(runes); {
  end := start + opts.MaxChars
  if end >= len(runes) {
   out = append(out, string(runes[start:]))
   break
  }
  cut := end
  floor := start + opts.MaxChars/2
  for i := end - 1; i > floor; i-- {
   if isBreakRune(runes[i]) {
    cut = i + 1 // 在空白/标点之后下刀，标点留在当前块内
    break
   }
  }
  out = append(out, string(runes[start:cut]))
  next := cut - opts.OverlapChars
  if next <= start {
   next = cut
  }
  start = next
 }
 return out
}

// isBreakRune 判断是否可以在此 rune 之后下刀：任意空白（unicode.IsSpace 覆盖
// 空格/换行/制表符）或中英文标点。英文靠空格保护单词完整性，中文没有空格，
// 退到标点（，。；等）后面切，比随机位置切断语义损伤小。
func isBreakRune(r rune) bool {
 if unicode.IsSpace(r) {
  return true
 }
 return strings.ContainsRune("，。！？；：、,.!?;:—…", r)
}
```

### 进阶测试（追加到 `chunk_test.go` 同包，共 6 个）

```go
// TestChunkRefined_SentenceBoundary 验证句级二级拆分：
// 超长段落按句打包，每个块的边界都落在句末标点/换行上，句子不被拦腰切断。
func TestChunkRefined_SentenceBoundary(t *testing.T) {
 // 8 个 30 rune 的句子（29 字 + "。"），段落共 240 rune > MaxChars=100
 var sb strings.Builder
 for i := 0; i < 8; i++ {
  sb.WriteString(strings.Repeat(string(rune('甲'+i)), 29))
  sb.WriteString("。")
 }
 para := sb.String()
 opts := ChunkOptions{MaxChars: 100, OverlapChars: 20}

 chunks := ChunkRefined(para, opts)
 if len(chunks) < 2 {
  t.Fatalf("len(chunks) = %d, want >= 2", len(chunks))
 }
 for i, c := range chunks {
  if runeLen(c) > opts.MaxChars {
   t.Errorf("chunks[%d] = %d rune，超过 MaxChars=%d", i, runeLen(c), opts.MaxChars)
  }
  // 句级打包（无重叠），每块必须以句末标点结尾 = 句子不被劈开
  if !strings.HasSuffix(c, "。") {
   t.Errorf("chunks[%d] 未落在句子边界上：尾部 %q", i, string([]rune(c)[runeLen(c)-5:]))
  }
 }
 // 句级路径无重叠，直接拼接必须还原原文（内容全覆盖）
 if got := strings.Join(chunks, ""); got != para {
  t.Errorf("句级切块拼接后与原文不一致：got %d rune, want %d", runeLen(got), runeLen(para))
 }
}

// TestChunkRefined_EnglishWordNotSplit 验证词边界回退：
// 单句超长（无句末标点）时走边界感知硬切，英文单词不被劈开；
// 同时与基础版 hardCut 对比，证明基础版确实会劈单词。
func TestChunkRefined_EnglishWordNotSplit(t *testing.T) {
 // "abcdefg " 8 rune 一组，重复 40 次 = 320 rune，单句（无标点）超 MaxChars=100。
 // 窗口右端 100 落在第 13 个词的中间（96..103 是 "abcdefg "），
 // 基础版硬切得到 "abcd|efg"，进阶版应回退到 96（空格后）下刀。
 // 注意：段落入口的 TrimSpace 会去掉尾部空格，实际参与切块的是 319 rune。
 para := strings.TrimSpace(strings.Repeat("abcdefg ", 40))
 opts := ChunkOptions{MaxChars: 100, OverlapChars: 20}

 refined := ChunkRefined(para, opts)
 if len(refined) < 2 {
  t.Fatalf("len(chunks) = %d, want >= 2", len(refined))
 }
 for i, c := range refined {
  if runeLen(c) > opts.MaxChars {
   t.Errorf("chunks[%d] = %d rune，超过 MaxChars=%d", i, runeLen(c), opts.MaxChars)
  }
  // 非末尾块必须切在空白/标点后（不劈单词）；末尾块终于文本结尾，豁免
  if i < len(refined)-1 && !endsAtBreakRune(c) {
   t.Errorf("chunks[%d] 末尾劈开了单词：尾部 %q", i, string([]rune(c)[max(0, runeLen(c)-6):]))
  }
 }
 if runeLen(refined[0]) != 96 {
  t.Errorf("首块 = %d rune，want 96（应回退到空格处下刀）", runeLen(refined[0]))
 }

 // 对比：基础版硬切在同一文本上的首块恰好 100 rune，且以 "abcd" 结尾（劈词实锤）
 base := hardCut(para, opts)
 if runeLen(base[0]) != 100 || !strings.HasSuffix(base[0], "abcd") {
  t.Errorf("对照组异常：基础版首块 = %d rune，尾部 %q（预期 100 rune 且劈开单词）",
   runeLen(base[0]), string([]rune(base[0])[94:]))
 }
}

// endsAtBreakRune 判断块是否切在空白/标点之后（即没有把一个单词从中间劈开）。
func endsAtBreakRune(chunk string) bool {
 runes := []rune(chunk)
 return isBreakRune(runes[len(runes)-1])
}

// TestChunkRefined_OverlapStillCorrect 验证边界回退后重叠逻辑仍正确：
// 相邻块恰好重叠 OverlapChars 个 rune，去重叠拼接还原原文。
func TestChunkRefined_OverlapStillCorrect(t *testing.T) {
 // TrimSpace 与段落入口行为对齐（实际切块输入是去掉尾部空格的 319 rune）
 para := strings.TrimSpace(strings.Repeat("abcdefg ", 40))
 opts := ChunkOptions{MaxChars: 100, OverlapChars: 20}

 chunks := ChunkRefined(para, opts)
 if len(chunks) < 2 {
  t.Fatalf("len(chunks) = %d, want >= 2", len(chunks))
 }
 for i := 0; i+1 < len(chunks); i++ {
  cur, next := []rune(chunks[i]), []rune(chunks[i+1])
  ov := opts.OverlapChars
  if len(cur) < ov || len(next) < ov {
   t.Fatalf("块长度不足以容纳重叠：chunks[%d]=%d, chunks[%d]=%d", i, len(cur), i+1, len(next))
  }
  if string(cur[len(cur)-ov:]) != string(next[:ov]) {
   t.Errorf("chunks[%d] 与 chunks[%d] 之间重叠不正确", i, i+1)
  }
 }
 var sb strings.Builder
 sb.WriteString(chunks[0])
 for _, c := range chunks[1:] {
  sb.WriteString(string([]rune(c)[opts.OverlapChars:]))
 }
 if sb.String() != para {
  t.Errorf("去重叠拼接后与原文不一致：got %d rune, want %d", runeLen(sb.String()), runeLen(para))
 }
}

// TestChunkRefined_ChinesePunctuationRetreat 验证中文场景：
// 单句超长但内部有逗号时，硬切回退到逗号后下刀（比随机位置更不伤语义）。
func TestChunkRefined_ChinesePunctuationRetreat(t *testing.T) {
 // 每 10 字一个逗号，无句末标点 → 单句 200+ rune，走 hardCutBoundary
 var sb strings.Builder
 for i := 0; i < 20; i++ {
  sb.WriteString(strings.Repeat("汉", 10))
  sb.WriteString("，")
 }
 para := sb.String() // 220 rune
 opts := ChunkOptions{MaxChars: 100, OverlapChars: 20}

 chunks := ChunkRefined(para, opts)
 for i, c := range chunks {
  if runeLen(c) > opts.MaxChars {
   t.Errorf("chunks[%d] = %d rune，超过 MaxChars=%d", i, runeLen(c), opts.MaxChars)
  }
 }
 // 逗号在 index 10,21,...,98（0-based）。窗口 [0,100) 的右端 100 落在 "汉" 上，
 // 向左回退找到 index 98 的逗号 → cut=99，首块以逗号结尾。
 if runeLen(chunks[0]) != 99 || !strings.HasSuffix(chunks[0], "，") {
  t.Errorf("首块 = %d rune，尾部 %q；预期 99 rune 且以逗号结尾",
   runeLen(chunks[0]), string([]rune(chunks[0])[max(0, runeLen(chunks[0])-3):]))
 }
}

// TestChunkRefined_SmallParagraphsSameAsBase 验证未触发超限时，
// 进阶版与基础版行为完全一致（段落打包路径不变，无回归）。
func TestChunkRefined_SmallParagraphsSameAsBase(t *testing.T) {
 text := "第一段：甲乙丙丁。\n\n第二段：戊己庚辛。\n\n第三段：壬癸。"
 opts := ChunkOptions{MaxChars: 100, OverlapChars: 10}

 base := Chunk(text, opts)
 refined := ChunkRefined(text, opts)
 if len(base) != len(refined) {
  t.Fatalf("块数不同：base=%d, refined=%d", len(base), len(refined))
 }
 for i := range base {
  if base[i] != refined[i] {
   t.Errorf("chunks[%d] 不同：base=%q, refined=%q", i, base[i], refined[i])
  }
 }
}

// TestChunkRefined_NoDeadLoop 验证边界回退不会死循环：
// 窗口内完全找不到空白/标点时退化为硬切，且 start 严格前进。
func TestChunkRefined_NoDeadLoop(t *testing.T) {
 para := strings.Repeat("x", 500) // 无任何 break rune
 done := make(chan []string, 1)
 go func() {
  done <- ChunkRefined(para, ChunkOptions{MaxChars: 100, OverlapChars: 20})
 }()
 select {
 case chunks := <-done:
  if len(chunks) == 0 {
   t.Error("无边界可退时仍应硬切产块")
  }
  for i, c := range chunks {
   if runeLen(c) > 100 {
    t.Errorf("chunks[%d] = %d rune，超过 MaxChars=100", i, runeLen(c))
   }
  }
 case <-time.After(2 * time.Second):
  t.Fatal("2 秒未返回：边界回退导致死循环")
 }
}
```

### 为什么这么写 / 易错处

1. **句级拆分只对超长段落做，不是所有文本**：装得下的段落整段打包已经是很好的边界，
   再拆句纯属浪费（拆句 + 逐句计长的 CPU 开销与文本长度成正比）。只对超长段落支付
   精细化的成本，是"懒加载"式的工程取舍。
2. **拆句不能 TrimSpace**：这是实测中真实踩到的坑——第一版测试直接拼接块与原文对比
   失败，因为段落入口 `splitParagraphs` 会 TrimSpace（尾部空格被剥掉），且句级拆分若
   再 TrimSpace 会丢失英文句间空格。**做覆盖性校验时，对照基准必须是"经过同样预处理
   的原文"，而不是原始输入**。
3. **回退必须有下限、起点必须严格前进**：没有 floor，边界恰在窗口开头时块会退化得过小；
   没有 `next > start` 钳制，重叠会在极端情况下把起点拉回去造成死循环。这两个防御和
   基础版的 overlap 钳制是同一思路：**不变式（step >= 1、start 单调前进）必须在循环
   内部就被保证，而不是靠调用方传好参数**。
4. **末尾块豁免边界检查**：最后一个块终于文本结尾，文本结尾不一定是空白/标点
   （TrimSpace 后一定不是），测试断言"每块都落在边界上"时必须豁免末尾块。

### 与基础版的取舍（面试怎么讲）

- **句级拆分的成本**：多一遍逐 rune 扫描（O(n)，可忽略），真正的成本是**块大小的
  方差变大**——句长不齐时块可能明显小于 MaxChars（块数变多，embedding 调用次数和
  成本上升），且句级路径放弃了块间重叠（句子是完整单元，重叠收益小，但极长句横跨
  语义时保护变弱）。
- **什么文档值得开**：段内长句多、句子是检索最小语义单元的文档——新闻、论文、法律
  条文、聊天记录；段落本来就短小的笔记类/列表类文档开了也白开（段落打包路径直接
  命中）。英文文档强烈建议开边界回退（劈单词直接伤 embedding 质量）；中文文档边界
  回退收益较小（汉字单字成词），退到标点主要是"看着舒服"级别的提升。
- **基础版够用的场景**：教学、以中文短段落为主的知识库、对切块质量不敏感的原型期。
  基础版的价值恰恰是"30 行讲清两级策略"，进阶版是生产化的下一步——这也是为什么
  两者以 `Chunk`/`ChunkRefined` 共存而不是互相替代。

### 加分项 ③：按字符而非 token 计数——开放讨论，无需实现

**本项为开放讨论，无需实现。** 理由：token 精确计数必须引入 tokenizer 库（如 tiktoken
的 Go 绑定）并加载模型对应词表，这与本工作区"先手写原理、不引入重型依赖"的约定直接
冲突；且 embedding 用 bge-m3、LLM 用 DeepSeek，两者词表不同，"按哪个模型的 token
算"本身就是一个没有唯一答案的 trade-off。面试能讲清这层即可：教学默认 400 字符 ≈
中文 200~270 token / 英文 100 token（换算经验值见骨架 `DefaultChunkOptions` 注释），
落在常见起点 200~500 token 的保守一侧；生产做法是把 `MaxChars` 换成 `MaxTokens` +
tokenizer 计数，切块策略本身（段落/句子/边界回退）完全复用。

## 四、对照清单

完成后逐条自评（不要求与答案一字不差，覆盖条目即可）：

- [ ] 段落按空行切分，TrimSpace 后丢弃空段；空输入 / 全空白输入返回 nil 或空切片，不产空块
- [ ] 贪心打包：装得下的段落合并进同一块（含 `\n\n` 分隔符计入长度），装不下时封存开新块，**段落不被拆散**
- [ ] 单段超过 MaxChars 时先 flush 当前块、再对该段硬切，文档顺序不错乱
- [ ] 硬切相邻块重叠恰好 OverlapChars 个 rune（末尾块可例外），去重叠拼接能还原原文（内容全覆盖）
- [ ] 所有长度判断与切片都基于 `[]rune`，纯中文文本切块后是合法 UTF-8、无乱码
- [ ] OverlapChars >= MaxChars（及负数、零值 opts）有防御，不死循环、有合理兜底
- [ ] 每个产出块都不超过 MaxChars（rune 计）
- [ ] `go vet ./...` 和 `go test ./internal/rag/` 全绿
- [ ] 能口头回答：为什么结构优先于固定窗口？为什么按 rune 不按 byte？字符与 token 的换算经验值？chunk 太大/太小各自伤什么？
- [ ] （进阶，可选）句级二级拆分：超长段落先按句末标点/换行拆句再贪心打包，块边界落在句子上；拆句不 TrimSpace，拼接能无损还原
- [ ] （进阶，可选）边界感知硬切：窗口内向左回退到最近空白/标点，英文单词不被劈开；回退有下限（floor）、起点严格前进（next > start），无边界可退时退化为硬切且不死循环
- [ ] （进阶，可选）能口头回答取舍：句级拆分的成本是什么？什么文档类型值得开？中文开边界回退的收益为什么比英文小？token 计数为什么不在这个项目里实现？
