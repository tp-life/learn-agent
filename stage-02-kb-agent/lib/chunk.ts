/**
 * lib/chunk.ts —— 文档切分（chunking）：把长文档切成适合 embedding 的块。
 *
 * 在 RAG 链路中的位置：文档解析之后、embedding 之前。
 * chunk 是"RAG 的第一调参位"：块太大 → 噪声多、相似度被稀释；
 * 块太小 → 上下文不完整、答案断章取义。
 *
 * 本模块与 Go 侧 mini-agent/internal/rag/chunk.go（练习 3）策略一致，
 * 做练习时建议先回顾 Go 侧练习 3 的参考答案
 * （docs/solutions/stage-02/exercise-3-chunking.md），本练习是它的 TS 移植。
 */

/** ChunkOptions 控制切分粒度。 */
export interface ChunkOptions {
  /** 每块最大字符数（按码点计，见 chunk 的 TODO 提示）。 */
  maxChars: number;
  /** 硬切时相邻块的重叠字符数，防上下文在边界被切断。 */
  overlapChars: number;
}

/**
 * DEFAULT_CHUNK_OPTIONS 经验起点：400 字符 + 15% 重叠。
 * 按字符而非 token 计数是简化（中文 1 字 ≈ 1-2 token，英文 1 词 ≈ 1-2 token），
 * 学习项目够用；生产环境会按 tokenizer 精确计数。
 */
export const DEFAULT_CHUNK_OPTIONS: ChunkOptions = {
  maxChars: 400,
  overlapChars: 60,
};

// TODO(练习6): 文档切分 chunk
//
// 【任务】实现：
//
//   export function chunk(text: string, options?: Partial<ChunkOptions>): string[]
//
// 把一篇文档切成若干不超过 maxChars 字符的块。策略与 Go 侧练习 3 完全一致——
// "结构优先，窗口兜底"：
//  1. 先按空行（"\n\n"）切段落，TrimSpace 后丢弃空段，再贪心地把整段
//     打包进块（段落不被拆散；段落间 "\n\n" 分隔符也占块内额度）；
//  2. 单段自身超过 maxChars 时，先封存当前块，再对该段做固定窗口硬切
//     （步长 = maxChars - overlapChars）。
// 防御：options 缺省时用 DEFAULT_CHUNK_OPTIONS 补齐；overlapChars 必须钳制到
// [0, maxChars-1]，否则步长为 0 或负数，硬切循环永不前进（死循环）。
//
// 【提示】
//   - 本练习最核心的坑：JS 字符串按 UTF-16 code unit 索引，不是按"字符"！
//     str.length、str.slice()、str[i] 都以 code unit 为单位。
//     中文等 BMP 字符恰好 1 个 code unit，看起来安全；
//     但 emoji 和很多生僻字是代理对（2 个 code unit），
//     按 code unit 切会把一个 emoji 劈成两个非法的"半个字符"（乱码）。
//     做法：切分和计量都基于码点数组 —— const chars = Array.from(text)
//     （或 [...text]，for..of 同理），chars.length 才是"字符数"，
//     切完用 chars.slice(start, end).join("") 拼回字符串。
//     这与 Go 侧"按 rune 不按 byte"是同一个坑在不同语言的形态。
//   - 结构参考 Go 侧练习 3 答案的三个函数：normalize（参数钳制）、
//     splitParagraphs（按空行切段）、hardCut（固定窗口硬切）+
//     主函数贪心打包。TS 版可以完全同构。
//   - 覆盖性不变量：除被 trim 掉的空白外，原文任何一段文字都应出现在
//     至少一个块里。自检方法：硬切块去重叠拼接后应还原原段落。
//
// 【验收】启动 dev server 后用 curl 上传样例文档（见
// app/api/ingest/route.ts 的 TODO 验收），返回的块数符合预期；
// 无 key 时配合 EMBEDDING_MOCK=1 跑通全链路。

const codePoints = (s: string): string[] => Array.from(s);

// 参考答案：docs/solutions/stage-02/exercise-6-ingest-pipeline.md（完成后再看）
export function chunk(text: string, options?: Partial<ChunkOptions>): string[] {
  const opts: ChunkOptions = normalizeChunkOptions(options);

  const chunk: string[] = [];
  let cur: string[] = [];
  let curLen = 0;

  const flush = () => {
    if (cur.length == 0) return;

    chunk.push(cur.join("\n\n"));
    cur = [];
    curLen = 0;
  };

  for (const para of splitParagraphs(text)) {
    const paraLen = codePoints(para).length;
    if (paraLen > opts.maxChars) {
      flush();
      chunk.push(...hardCut(para, opts));
      continue;
    }
    const addLen = paraLen + (cur.length > 0 ? 2 : 0);
    if (curLen + addLen > opts.maxChars) {
      flush();
    }

    curLen = cur.length === 0 ? paraLen : curLen + 2 + paraLen;
    cur.push(para);
  }
  flush();
  return chunk;
}

function splitParagraphs(text: string): string[] {
  return text
    .split("\n\n")
    .map((p) => p.trim())
    .filter((p) => p !== "");
}

function hardCut(para: string, opts: ChunkOptions): string[] {
  const chars = codePoints(para);
  const step = opts.maxChars - opts.overlapChars;
  const out: string[] = [];
  for (let start = 0; start < chars.length; start += step) {
    const end = Math.min(start + opts.maxChars, chars.length);
    out.push(chars.slice(start, end).join(""));
    if (end === chars.length) {
      break;
    }
  }
  return out;
}

function normalizeChunkOptions(options?: Partial<ChunkOptions>): ChunkOptions {
  const opts: ChunkOptions = { ...DEFAULT_CHUNK_OPTIONS, ...options };
  if (opts.maxChars <= 0) {
    opts.maxChars = DEFAULT_CHUNK_OPTIONS.maxChars;
  }

  if (opts.overlapChars < 0) {
    opts.overlapChars = 0;
  }

  if (opts.overlapChars >= opts.maxChars) {
    opts.overlapChars = opts.maxChars - 1;
  }
  return opts;
}
