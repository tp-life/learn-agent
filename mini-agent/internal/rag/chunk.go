// Package rag 实现 RAG（Retrieval-Augmented Generation）链路中"文档侧"的能力。
//
// 在 agent 链路中的位置：RAG = 文档 → 切块（chunking）→ embedding 入库 →
// 检索 top-k → 把相关块组装进 prompt 交给 LLM。整条链路横跨多个包：
//   - 本包（internal/rag）：切块（本文件，练习 3）与检索工具 kb_search（练习 4）；
//   - internal/embed（练习 1）：把文本翻译成向量；
//   - internal/vectorstore（练习 2）：存向量并做相似度检索。
//
// 为什么切块单独成章：LLM 的上下文窗口和 embedding 模型的输入长度都有限，
// 文档不能整篇塞进 prompt，也不能整篇只 embedding 成一个向量（主题会被稀释）。
// 切块质量直接决定检索质量的上限——检索回来的块如果断章取义或充满噪声，
// 后面的 prompt 工程做得再好也救不回来。
package rag

import (
	"strings"
)

// ChunkOptions 是文档切块的调参入口。
//
// MaxChars 是 RAG 的第一调参位（面试高频：chunk 切多大？）：
//   - 太大 → 一个块里混入多个主题，embedding 向量被"平均"稀释，检索命中率下降；
//   - 太小 → 上下文不完整，答案断章取义（"第 3 条"被切走，块里只剩"……除外"）。
//
// OverlapChars 是相邻块之间的重叠长度，防止语义在块边界被拦腰切断：
// 一句话横跨两个块时，重叠保证它在至少一个块里是完整的。
// 常见经验值是块大小的 10%~20%。
type ChunkOptions struct {
	MaxChars     int // 每个块的最大字符数（rune 计）
	OverlapChars int // 相邻硬切块之间的重叠字符数（rune 计）
}

// DefaultChunkOptions 返回教学用默认值：400 字符块 + 60 字符重叠。
//
// 注意：这里按字符（rune）而不是 token 计数，是有意的简化——
// 教学项目不引入 tokenizer 库（如 tiktoken），token 精确计数需要
// 加载模型对应的词表文件，成本高且与"先手写原理"的约定相悖。
// 面试可主动讲 token 与字符的换算经验值：
// 中文 1 token ≈ 1.5~2 字符，英文 1 token ≈ 4 字符。
// 因此 400 字符 ≈ 中文 200~270 token / 英文 100 token，
// 落在常见起点（200~500 token）的偏保守一侧，对 bge-m3
// （最大输入 8192 token）非常安全。
func DefaultChunkOptions() ChunkOptions {
	return ChunkOptions{MaxChars: 400, OverlapChars: 60}
}

// TODO(练习3): chunking 文档切分
//
// 【任务】
// 实现 func Chunk(text string, opts ChunkOptions) []string：
// 把一篇文档切成若干不超过 opts.MaxChars 字符（rune）的块，供后续
// embedding 入库。签名已给出，你只需要填实现。
//
// 【提示】
//   - 策略：结构优先。先按空行把文本切成段落（strings.Split(text, "\n\n")，
//     每段 TrimSpace、丢弃空段），再贪心地把段落依次打包进一个块：
//     当前块放得下（连同段落间分隔符 "\n\n" 一起计）就追加，放不下就
//     封存当前块、开新块。段落不拆——结构完整的块检索质量最好。
//   - 单段自身超过 MaxChars 时，对该段硬切（固定窗口）：从 rune 偏移
//     start 处取 MaxChars 个 rune 为一块，下一块从 start + MaxChars -
//     OverlapChars 处开始，即相邻块重叠 OverlapChars 个 rune。
//   - 最大的坑：必须用 []rune 切片计量和切分，绝不能用 byte 下标——
//     中文一个字符占 3 个 byte，按 byte 切会把一个汉字从中间劈开，
//     产出乱码（面试常考的 UTF-8 考点）。
//   - 防御 opts 异常值：MaxChars <= 0 时用 DefaultChunkOptions() 兜底；
//     OverlapChars >= MaxChars 时硬切的步长（MaxChars - OverlapChars）
//     会变成 0 或负数导致死循环，必须把 overlap 钳制到 [0, MaxChars-1]。
//   - 不产空块（每块 TrimSpace 后为空则丢弃）；尽量保证原文内容全覆盖
//     （除被 TrimSpace 掉的空白外，任何一段文字都应出现在至少一个块里）。
//
// 【验收】
// go test ./internal/rag/ 通过（测试由参考答案提供，需覆盖：多段落打包
// 不超限不拆段、超长段硬切且重叠正确、纯中文无乱码、空输入返回 nil、
// OverlapChars >= MaxChars 不死循环）。
//
// 参考答案：docs/solutions/stage-02/exercise-3-chunking.md（完成后再看）
func Chunk(text string, opts ChunkOptions) []string {
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
		paraLen := len([]rune(para))
		if len(para) > opts.MaxChars {
			flush()
			chunks = append(chunks, hardCut(para, opts)...)
			continue
		}
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

func splitParagraphs(text string) []string {
	var paras []string

	for _, p := range strings.Split(text, "\n\n") {
		if p = strings.TrimSpace(p); p != "" {
			paras = append(paras, p)
		}
	}
	return paras
}

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
			break
		}
	}
	return out
}
