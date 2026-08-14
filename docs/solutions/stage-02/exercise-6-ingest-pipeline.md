# 练习 6 参考答案：ingest 流水线（chunking + embedding + 入库）

> 对应 TODO：`stage-02-kb-agent/lib/chunk.ts` 的 `TODO(练习6)`（chunk）、
> `stage-02-kb-agent/lib/embed.ts` 的 `TODO(练习6)`（embedTexts）、
> `stage-02-kb-agent/app/api/ingest/route.ts` 的 `TODO(练习6)`（组装逻辑）。
> **完成练习并自评后再看本文档。**
>
> 本文档代码已于 2026-08-06 实际粘贴进项目验证（验证后项目代码已恢复为骨架版）：
>
> - `pnpm build` 通过（Next.js 16.3.0，生产构建含类型检查全绿）；
> - `EMBEDDING_MOCK=1 pnpm dev` 启动后实测：
>   - `curl -F "file=@sample.md" localhost:3000/api/ingest`（多段落中文 md）
>     → `{"ok":true,"file":"kb-sample.md","chunks":1,"total":1}`，`data/kb.json` 生成，
>     内含 1024 维向量与 `{source, chunk}` 元数据；
>   - 含 680 码点超长单段（无空行）+ emoji 的文档 → `chunks:4`：
>     标题段 5 码点、硬切块 400 + 340 码点（相邻重叠恰好 60 码点）、结尾段 11 码点，
>     全部块 `isWellFormed()` 通过（emoji 代理对未被切坏）；
>   - 负路径：上传 `.pdf` → 400；裸 POST（非 multipart）→ 400；重复上传可重复入库（见设计点 6）。
> - 验证产生的 `data/kb.json` 与临时样例文件已清理。

---

## 一、参考实现

### `lib/chunk.ts`（完整替换骨架；文件头部包注释与类型定义不变，此处给出全文）

```ts
/**
 * lib/chunk.ts —— 文档切分（chunking）：把长文档切成适合 embedding 的块。
 *
 * 在 RAG 链路中的位置：文档解析之后、embedding 之前。
 * chunk 是"RAG 的第一调参位"：块太大 → 噪声多、相似度被稀释；
 * 块太小 → 上下文不完整、答案断章取义。
 *
 * 本模块与 Go 侧 mini-agent/internal/rag/chunk.go（练习 3）策略一致。
 */

/** ChunkOptions 控制切分粒度。 */
export interface ChunkOptions {
  /** 每块最大字符数（按码点计，见 chunk 实现注释）。 */
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

/**
 * codePoints 把字符串展开为码点数组。
 *
 * 本模块最核心的坑：JS 字符串按 UTF-16 code unit 索引，不是按"字符"！
 * str.length、str.slice()、str[i] 都以 code unit 为单位。
 * 中文等 BMP 字符恰好 1 个 code unit，看起来安全；
 * 但 emoji 和很多生僻字是代理对（2 个 code unit），
 * 按 code unit 切会把一个 emoji 劈成两个非法的"半个字符"（乱码）。
 * Array.from / [...s] / for..of 都按码点迭代，是安全的展开方式——
 * 这与 Go 侧"按 rune 不按 byte"是同一个坑在不同语言的形态。
 */
const codePoints = (s: string): string[] => Array.from(s);

/**
 * chunk 把一篇文档切成若干不超过 maxChars 字符（码点）的块。
 *
 * 策略是"结构优先，窗口兜底"：
 *  1. 先按空行切段落，再贪心地把整段打包进块，段落不被拆散；
 *  2. 单段自身超过 maxChars 时，才对该段做固定窗口硬切（带重叠）。
 */
export function chunk(text: string, options?: Partial<ChunkOptions>): string[] {
  const opts = normalizeChunkOptions(options);

  const chunks: string[] = [];
  let cur: string[] = []; // 当前块已打包的段落
  let curLen = 0; // 当前块的码点数（含段落间 "\n\n" 分隔符）

  // flush 封存当前块，同时重置打包状态。
  const flush = () => {
    if (cur.length === 0) return;
    chunks.push(cur.join("\n\n"));
    cur = [];
    curLen = 0;
  };

  for (const para of splitParagraphs(text)) {
    const paraLen = codePoints(para).length;
    if (paraLen > opts.maxChars) {
      // 超长段落：先封存当前块（保持段落顺序），再对该段硬切。
      flush();
      chunks.push(...hardCut(para, opts));
      continue;
    }
    const addLen = paraLen + (cur.length > 0 ? 2 : 0); // "\n\n" 分隔符也占块内额度
    if (curLen + addLen > opts.maxChars) {
      flush(); // 当前块放不下，封存后开新块
    }
    curLen = cur.length === 0 ? paraLen : curLen + 2 + paraLen;
    cur.push(para);
  }
  flush();

  return chunks;
}

/**
 * normalizeChunkOptions 用默认值补齐缺省字段，并钳制异常参数，
 * 保证硬切循环一定收敛：overlapChars 必须落在 [0, maxChars-1]，
 * 否则步长 maxChars-overlapChars 为 0 或负数，for 循环永不前进（死循环）。
 * 不变式在入口处建立，下游就不用重复防御。
 */
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

/**
 * splitParagraphs 按空行切段，trim 后丢弃空段——空段没有语义，
 * 留着只会产出让 embedding 白跑一次的空块。
 */
function splitParagraphs(text: string): string[] {
  return text
    .split("\n\n")
    .map((p) => p.trim())
    .filter((p) => p !== "");
}

/**
 * hardCut 对超长段落做固定窗口切分：每块最多 maxChars 个码点，
 * 相邻块重叠 overlapChars 个码点（步长 = maxChars - overlapChars）。
 * 参数已经过 normalizeChunkOptions 钳制，step 必然 >= 1。
 * 全程基于码点数组切分（见 codePoints 的注释），切完 join 回字符串。
 */
function hardCut(para: string, opts: ChunkOptions): string[] {
  const chars = codePoints(para);
  const step = opts.maxChars - opts.overlapChars;
  const out: string[] = [];
  for (let start = 0; start < chars.length; start += step) {
    const end = Math.min(start + opts.maxChars, chars.length);
    out.push(chars.slice(start, end).join(""));
    if (end === chars.length) {
      break; // 末尾块通常不足 maxChars，与上一块的重叠会超过 overlapChars，属正常
    }
  }
  return out;
}
```

### `lib/embed.ts`（完整替换骨架；文件头部包注释不变，此处给出全文）

```ts
/**
 * lib/embed.ts —— embedding 客户端：把文本变成向量（RAG 的"翻译器"）。
 *
 * 在 RAG 链路中的位置：chunking 之后、向量库之前；
 * 写入路径（embed 每个 chunk）和查询路径（embed 用户问题）共用本模块，
 * 两侧必须用同一个 embedding 模型，向量空间才对齐。
 *
 * 本模块与 Go 侧 mini-agent/internal/embed（练习 1）对应：
 * DeepSeek 官方没有 embedding API，embedding 必须走另一家服务商——
 * 这里直接 fetch 硅基流动的 OpenAI 兼容接口（POST /v1/embeddings，
 * 模型 BAAI/bge-m3），不引 SDK：批量接口就是一个 POST + JSON，
 * 手写几十行就能说清楚，引 SDK 反而遮住"按 index 归位"这个核心细节。
 */

/**
 * bge-m3 的输出维度。写死它的意义：入库前校验向量长度，
 * 维度错了说明模型/服务商配错了，越早报错越好——
 * 否则错误向量悄悄入库，检索结果全错还很难排查。
 */
export const BGE_M3_DIMENSIONS = 1024;

const SILICONFLOW_EMBEDDINGS_URL = "https://api.siliconflow.cn/v1/embeddings";
const EMBEDDING_MODEL = "BAAI/bge-m3";

/**
 * embedTexts 输入一批文本，返回与之一一对应的向量数组
 * （result[i] 是 texts[i] 的向量）。
 *
 * 两条路径：
 *  1. EMBEDDING_MOCK=1：返回确定性假向量，不调用真实 API（见 mockEmbedding）。
 *  2. 默认：fetch 硅基流动批量 embedding 接口。
 *
 * 核心坑（与 Go 侧相同）：响应 data 数组的顺序不能假设与输入一致！
 * 每个元素带 index 字段标明它对应 input 的第几段，归位必须按 index 放，
 * 不能按数组下标直接对应（部分服务商会按内部策略重排，文档不保证顺序）。
 *
 * 已知局限：真实项目里文档很大时要分批调用（硅基流动单批有 token 数上限），
 * 本实现一次请求全部 chunk——学习项目文档小，够用；超限会收到 4xx，
 * 错误信息里能看到。
 */
export async function embedTexts(texts: string[]): Promise<number[][]> {
  if (texts.length === 0) {
    throw new Error("embed: empty input");
  }
  for (let i = 0; i < texts.length; i++) {
    if (texts[i].trim() === "") {
      throw new Error(`embed: texts[${i}] is empty`);
    }
  }

  // mock 开关放在最前面：不碰网络，无需 key。
  if (process.env.EMBEDDING_MOCK === "1") {
    return texts.map(mockEmbedding);
  }

  const apiKey = process.env.SILICONFLOW_API_KEY;
  if (!apiKey) {
    throw new Error(
      "embed: SILICONFLOW_API_KEY 未设置（没有 key 时可设 EMBEDDING_MOCK=1 走 mock 路径）",
    );
  }

  const resp = await fetch(SILICONFLOW_EMBEDDINGS_URL, {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
      Authorization: `Bearer ${apiKey}`,
    },
    body: JSON.stringify({ model: EMBEDDING_MODEL, input: texts }),
  });
  if (!resp.ok) {
    // 把状态码和响应体带进错误信息——排查限流/余额/模型名错误全靠它。
    throw new Error(`embed: HTTP ${resp.status}: ${await resp.text()}`);
  }

  const json = (await resp.json()) as {
    data?: { index: number; embedding: number[] }[];
  };
  const data = json.data ?? [];
  if (data.length !== texts.length) {
    throw new Error(
      `embed: got ${data.length} embeddings for ${texts.length} texts`,
    );
  }

  // 按 index 归位，而不是按数组顺序（见函数 doc 注释）。
  const result: number[][] = new Array(texts.length);
  for (const d of data) {
    if (d.index < 0 || d.index >= texts.length) {
      throw new Error(
        `embed: index ${d.index} out of range [0, ${texts.length})`,
      );
    }
    if (d.embedding.length !== BGE_M3_DIMENSIONS) {
      throw new Error(
        `embed: texts[${d.index}] dim = ${d.embedding.length}, want ${BGE_M3_DIMENSIONS}`,
      );
    }
    result[d.index] = d.embedding;
  }
  // 归位后无空洞校验：防止服务端漏返回某一段（否则空洞会带着
  // undefined 进向量库，错误潜伏到检索时才暴露）。
  for (let i = 0; i < result.length; i++) {
    if (!result[i]) {
      throw new Error(`embed: texts[${i}] missing in response`);
    }
  }

  return result;
}

/**
 * mockEmbedding 生成"确定性假向量"：同一段文本永远得到同一个向量。
 *
 * 为什么要有这个开关（可测试性教学点）：embedding 是外部依赖，
 * 没有 key / 没网 / 怕花钱时全链路就没法验证。用确定性的 fake 替换
 * 外部依赖，ingest 流水线（chunk → embed → 入库 → 落盘）可以离线
 * 端到端跑通；假向量没有语义（相似文本的向量并不相近），检索质量
 * 无意义，但"管线通不通"能验证。这正是单测里 mock/stub 思想的应用。
 *
 * 实现：文本 hash（FNV-1a）做种子，驱动 xorshift32 伪随机发生器，
 * 生成 BGE_M3_DIMENSIONS 维、取值 [-1, 1) 的向量。维度与真实 bge-m3
 * 一致，保证 mock 数据和真实数据可以走完全相同的下游校验逻辑。
 */
function mockEmbedding(text: string): number[] {
  // FNV-1a hash：简单、确定性、分布够散。codePointAt 按码点取，
  // 与 chunk 模块的码点纪律一致。
  let seed = 2166136261;
  for (const ch of text) {
    seed ^= ch.codePointAt(0)!;
    seed = Math.imul(seed, 16777619); // Math.imul：32 位整数乘法，防精度溢出
  }
  let s = seed >>> 0;
  if (s === 0) {
    s = 0x9e3779b9; // xorshift 种子不能为 0，否则永远输出 0，产生零向量
  }

  const v: number[] = [];
  for (let i = 0; i < BGE_M3_DIMENSIONS; i++) {
    // xorshift32：三步移位异或。>>> 是无符号右移，保证按 32 位无符号运算。
    s ^= s << 13;
    s ^= s >>> 17;
    s ^= s << 5;
    v.push(((s >>> 0) / 0x100000000) * 2 - 1); // 映射到 [-1, 1)
  }
  return v;
}
```

### `app/api/ingest/route.ts`（骨架只改两处，其余不变）

> 2026-08-06 注：练习 7 搭建时把 `getKbStore`/`KB_PATH` 从本文件挪到了
> `lib/kb.ts`（chat 路由也要用同一个单例），本答案的 import 块已同步更新；
> TODO 替换代码不变（`getKbStore()`、`KB_PATH` 只是改为从 `@/lib/kb` 导入）。

import 部分（骨架当前为 `import { NextResponse } from "next/server";`
和 `import { getKbStore, KB_PATH } from "@/lib/kb";` 两行）改为：

```ts
import { NextResponse } from "next/server";
import { chunk } from "@/lib/chunk";
import { embedTexts } from "@/lib/embed";
import { getKbStore, KB_PATH } from "@/lib/kb";
import type { Document } from "@/lib/vectorstore";
```

`TODO(练习6)` 块（含 `void getKbStore;` 和 501 返回）整段替换为：

```ts
// —— 组装写入路径：chunk → embed → 入库 → 落盘 ——

const chunks = chunk(text);
if (chunks.length === 0) {
  return NextResponse.json({ error: "文档切分结果为空" }, { status: 400 });
}

try {
  const vectors = await embedTexts(chunks);

  const store = getKbStore();
  // id 用 "文件名#块序号"：可读、可定位回来源块；
  // metadata 带溯源信息，练习 7 的"可点击引用"全靠它。
  const docs: Document[] = chunks.map((t, i) => ({
    id: `${file.name}#${i}`,
    text: t,
    vector: vectors[i],
    metadata: { source: file.name, chunk: String(i) },
  }));
  // add 是 all-or-nothing：任一文档校验失败整批 throw，库里不留中间状态。
  store.add(...docs);
  store.save(KB_PATH);

  return NextResponse.json({
    ok: true,
    file: file.name,
    chunks: chunks.length,
    total: store.size,
  });
} catch (err) {
  // embed 网络/鉴权失败、向量维度不符等都从这里兜住，
  // 把错误信息带回给调用方（排错全靠它），而不是只给一个裸 500。
  return NextResponse.json(
    { error: err instanceof Error ? err.message : String(err) },
    { status: 500 },
  );
}
```

## 二、关键设计点

1. **JS 的"rune 坑"是 UTF-16 code unit vs 码点**：Go 侧练习 3 的坑是 byte vs rune；TS 侧的对应形态是 `str.length`/`slice()` 按 UTF-16 code unit 计。中文 BMP 字符 1 unit = 1 码点，纯中文按 unit 切"看起来"没事——这让 bug 极隐蔽；emoji（代理对，2 unit）一下刀就碎。**易错处**：只把切片改成 `Array.from`，长度判断还残留 `str.length`——与 Go 侧"切片改了 rune、长度还按 byte"是同构错误。验证手段：对切出的块跑 `text.isWellFormed()`（ES2024），孤立代理项必现形。

2. **结构优先、窗口兜底两级策略全部继承自 Go 侧练习 3**：贪心打包保持段落完整；超长段先 flush 当前块再硬切（否则硬切块插到已打包段落前面，顺序错乱）；`overlapChars >= maxChars` 必须在入口钳制，否则步长为 0 死循环。这些不再重复展开，回读 `docs/solutions/stage-02/exercise-3-chunking.md` 的设计点 1/3/4/5。

3. **index 归位是 embedding 接入的第一坑**（与 Go 侧练习 1 相同）：响应 `data` 数组顺序不可假设，必须按 `index` 字段放回 `result[index]`，并校验不越界、无空洞、维度 = 1024。少任何一个校验，错误都会潜伏到检索阶段才暴露（文本和向量错位 → 检索结果全是错的，且不报任何错）。

4. **EMBEDDING_MOCK 的确定性是关键属性，不是随便返回随机数**：`Math.random()` 版的 mock 会让同一文本每次向量不同，重复上传后检索行为不可复现，调试时无法对比两次运行。用文本 hash 做种子的伪随机发生器保证"同文同向量"——这正是可测试性的核心：**fake 替换外部依赖后，系统行为必须仍然确定**。同时 mock 向量保持 1024 维，让 mock 数据和真实数据走完全相同的下游校验（维度不符整批拒绝的逻辑在 mock 下也被真实执行）。

5. **入库失败的中间状态**：`store.add` 是 all-or-nothing，所以 add 失败库不变；但 `add` 成功、`save` 失败（磁盘满等）时，内存库已含新块而磁盘没有——进程重启后数据丢失。本实现接受这个窗口（下次重新上传即可恢复）；生产做法是先 save 再响应，或定期 flush + WAL 思路。**易错处**：把 `save` 放在 try 外面，save 异常变成无错误信息的裸 500。

6. **重复上传不去重是有意的简化**：同一文件传两次，库里有两份块（id 相同、内容相同），检索结果会重复但不影响正确性。要处理的话思路是入库前按 `metadata.source` 过滤旧块——留给练习 8-9 调优阶段讨论，因为"更新文档"的语义（覆盖 vs 版本化）本身就是设计决策。

7. **骨架外壳已修的一个坑（验证中发现）**：`req.formData()` 对非 multipart 请求直接抛异常，不兜住就是无错误信息的裸 500。骨架已在解析处加 try/catch 返回 400——HTTP 入口的每个"假设输入合法"的调用都值得问一句：不合法时会怎样？

## 三、进阶实现：PDF 支持（2026-08-06 回补）

> 对应 route.ts TODO 提示里的"进阶要求：PDF 支持"。
> 本节代码已于 2026-08-06 实际粘贴进项目（叠加在第一节完整实现之上）验证，
> 验证后项目代码已恢复为骨架版、`unpdf` 依赖已移除。验证记录见本节末尾。

### 库选择：为什么是 unpdf 而不是 pdf-parse

TODO 提示早期版本写的是 pdf-parse（社区知名度最高的 PDF 文本提取库），
但它在 Next.js 里有一个**必现的坑**，选型时必须知道：

- **pdf-parse v1 的 `index.js` 带一段 debug 代码**：
  `isDebugMode = !module.parent`，为 true 时去读
  `./test/data/05-versions-space.pdf` 并打印结果。
  直接 `node -e "require('pdf-parse')"` 时 `module.parent` 存在，没事；
  但经 webpack/turbopack 打包后 `module.parent` 是 `undefined`
  → **import 那一刻就 ENOENT 崩溃**，业务代码一行都没跑到。
- 社区绕法：深引入 `pdf-parse/lib/pdf-parse.js` 绕过 `index.js`，
  但 `@types/pdf-parse` 只声明了 `pdf-parse` 主入口，
  深引入路径的类型声明要自己补一个 `.d.ts`。
- **unpdf**（unjs 出品，pdfjs-dist 的轻封装）为 serverless/Node 设计、
  ESM、自带 TypeScript 类型，没有上述任何坑——本答案选它。
  它在 Next.js 16（turbopack 默认）下无需任何额外配置即可工作
  （已实测，见验证记录）。

无论选哪个库，两条共性限制要清楚（面试常问）：

1. 纯文本提取对**扫描件（图片型 PDF）无能为力**——文字在像素里，
   需要 OCR（如 tesseract），那是另一条技术路线；
2. 提取质量取决于 PDF 内部结构，复杂排版（多栏、表格）的文本顺序
   可能是乱的，生产场景需要更重的解析方案。

### 完整代码（只需改 `app/api/ingest/route.ts`，chunk/embed/入库管线零改动）

在第一节完整实现的基础上改三处：

**1. import 块加一行**：

```ts
import { extractText } from "unpdf";
```

**2. 扩展名白名单放行 .pdf**（替换基础版的白名单判断）：

```ts
// 扩展名白名单：md/txt 是纯文本，file.text() 直接读；pdf 走 unpdf 解析。
const isPdf = /\.pdf$/i.test(file.name);
if (!isPdf && !/\.(md|txt)$/i.test(file.name)) {
  return NextResponse.json(
    {
      error: `暂不支持的文件类型：${file.name}（目前只支持 .md / .txt / .pdf）`,
    },
    { status: 400 },
  );
}
```

**3. 文本提取分支**（替换基础版的 `const text = await file.text();`）：

```ts
// PDF 不能用 file.text()——那是按 UTF-8 解码字节流，
// 得到的是乱码而不是文档文字；必须走解析库。
let text: string;
if (isPdf) {
  try {
    const result = await extractText(new Uint8Array(await file.arrayBuffer()), {
      mergePages: true, // 多页合并成一个字符串；默认按页返回 string[]
    });
    text = result.text;
  } catch (err) {
    // 解析失败（加密/损坏/非 PDF 内容改了扩展名）按 400 处理：
    // 是客户端给了一个没法解析的文件，不是服务端故障。
    return NextResponse.json(
      {
        error: `PDF 解析失败：${err instanceof Error ? err.message : String(err)}`,
      },
      { status: 400 },
    );
  }
} else {
  text = await file.text();
}
if (text.trim() === "") {
  // 扫描件（图片型 PDF）提取不出文字也会走到这里。
  return NextResponse.json({ error: "文件内容为空" }, { status: 400 });
}
```

之后的 `chunk(text)` → `embedTexts(chunks)` → 入库 → 落盘与基础版
**完全相同**——这正是管线设计的价值：新数据源只需要把"字节 → 文本"
这一段换掉，下游不用动。

### 易错处

1. **用 `file.text()` 读 PDF**：按 UTF-8 解码二进制字节流，产出一堆
   乱码还能通过"非空"校验，乱码文本直接进 embedding——静默错误，
   检索质量全毁且不报任何错。必须走 `arrayBuffer()` + 解析库。
2. **解析异常没兜住**：损坏/加密的 PDF 会让 `extractText` reject，
   不 try/catch 就是无错误信息的裸 500；而且应该返回 400
   （客户端文件有问题），不是 500。
3. **`mergePages`**：`extractText` 默认按页返回 `string[]`，
   直接拿去 chunk 会丢页间上下文的衔接（也好处理，但行为不同）；
   传 `{ mergePages: true }` 得到单一字符串，与 md/txt 路径对齐。
4. **大小写与伪装扩展名**：白名单正则带 `i` 标志；把 `.txt` 改名
   `.pdf` 上传会走到解析分支然后失败——这正是上面 400 兜底的场景。

### 与基础版的取舍

- 进阶版引入了一个外部依赖（unpdf 及其间接依赖 pdfjs-dist），
  换来 PDF 数据源支持；基础版零依赖、只能吃纯文本。
- PDF 提取的文本没有 markdown 结构（标题、列表丢失），分块只能依赖
  段落空行与硬切——同一份内容，PDF 版的检索质量通常低于 md 版。
  这是"数据源格式"的问题，不是管线的问题。
- 生产环境还会加：PDF 页数/大小限制（提取是 CPU 密集操作）、
  按页保留 metadata（引用可精确到页码）、OCR 兜底。

### 验证记录（2026-08-06）

环境：Next.js 16.3.0（turbopack 默认，无额外配置）、unpdf 1.8.0、
node v26。测试 PDF 为手写最小 PDF 结构（Catalog/Pages/Page/Font/
Contents 五个对象 + 计算正确的 xref 偏移表，标准 Helvetica，
ASCII 文本——避免 CID 字体嵌入的复杂度）。

- `pnpm typecheck` 通过；`pnpm build` 通过（含类型检查与静态生成全绿）。
- `EMBEDDING_MOCK=1 pnpm dev` 启动后实测：
  - `curl -F "file=@ex6-test.pdf" localhost:3000/api/ingest`
    → `{"ok":true,"file":"ex6-test.pdf","chunks":2,"total":2}`；
    `data/kb.json` 内含 2 条 1024 维向量记录，
    metadata 为 `{"source":"ex6-test.pdf","chunk":"0|1"}`，
    text 为 PDF 中提取的真实文字（非乱码）；
  - 负路径：上传 `.docx` → 400（白名单拒绝）；
    伪装成 `.pdf` 的纯文本文件 → 400 `PDF 解析失败：Invalid PDF structure.`；
  - 回归：上传中文 markdown（含 emoji）→ 正常入库，md 路径不受影响。
- 验证后已回滚：三个源文件恢复骨架版、`pnpm remove unpdf`
  （package.json 与 pnpm-lock.yaml 逐字节恢复原状）、
  `data/kb.json` 与临时测试文件已清理、骨架版 `pnpm build` 复测通过。

## 四、对照清单

完成后逐条自评（不要求与答案一字不差，覆盖条目即可）：

- [x] `chunk` 结构优先：按空行切段、trim 丢空段、贪心打包不拆散段落、`\n\n` 分隔符计入块长度
- [x] 超长段先 flush 再硬切；硬切相邻块重叠恰好 overlapChars（末尾块可例外）；去重叠拼接能还原原段落（覆盖性）
- [x] 所有长度判断与切片基于码点（`Array.from` / 展开 / `for..of`），含 emoji 的文档切完每个块 `isWellFormed()` 为 true
- [x] `overlapChars >= maxChars`、负数、缺省 options 都有防御，硬切不死循环
- [x] `embedTexts` 按响应 `index` 归位；index 越界、数量不符、维度 ≠ 1024、归位有空洞都报错
- [x] 响应非 200 时错误信息包含状态码与响应体
- [x] `EMBEDDING_MOCK=1` 路径不碰网络、无需 key，且**同一文本多次调用返回相同向量**（确定性）
- [x] ingest 组装：id 含来源信息、metadata 带 `{source, chunk}`、add/save 异常被兜住并返回带信息的 500、成功返回块数与库总量
- [x] `pnpm build` 通过；`EMBEDDING_MOCK=1 pnpm dev` 下 `curl -F "file=@sample.md" localhost:3000/api/ingest` 返回 `{"ok":true,...}` 且 `data/kb.json` 落盘
- [x] 能口头回答：JS 字符串为什么按 code unit 索引、emoji 为什么会被切坏？为什么 mock 必须确定性？为什么 embedding 响应要按 index 归位？chunk 太大/太小各自伤什么？
- [x] （进阶）PDF 分支：白名单放行 `.pdf`；用 `arrayBuffer()` + unpdf 提取文本（`mergePages: true`），解析失败返回带信息的 400 而非裸 500；提取出的文本与 md/txt 走**完全相同**的 chunk/embed/入库管线
- [x] （进阶）实测：手写或生成一个最小 PDF，`EMBEDDING_MOCK=1` 下 `curl -F` 上传返回 `{"ok":true,...}` 且 `data/kb.json` 中 text 是真实提取文字（非乱码）；伪装扩展名的坏文件被 400 兜住
- [x] （进阶）能口头回答：为什么不能 `file.text()` 读 PDF？pdf-parse 在 Next.js 打包下为什么 import 即崩、怎么绕？扫描件 PDF 为什么提取不出文字？
