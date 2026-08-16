# 第 7 章：全栈知识库 Agent 实战——Next.js + AI SDK 把 RAG 做成产品

> 对应阶段：阶段二（进阶）· 项目 2 `stage-02-kb-agent/`
> 代码位置：`stage-02-kb-agent/lib/`（RAG 底座 TS 版）、`app/api/ingest/route.ts`（写入路径）、`app/api/chat/route.ts`（查询路径）、`app/page.tsx` + `components/sources-card.tsx`（前端）
> 前置：第 4 章（embedding 与向量检索）、第 5 章（RAG 全链路）——这两章的概念在 Go 侧 `mini-agent/` 讲过，本章是它们的 TS 全栈落地
> 学完后你能讲清：一个可上传文档、流式回答、引用可展开的知识库产品是怎么拼起来的；Vercel AI SDK 的流式协议到底替你做了什么；为什么"同一套 RAG 底座用 Go 和 TS 各写一遍"本身就是面试弹药。

---

## 本章地图

- 项目 2 全景架构：浏览器 → Route Handler → 写入/查询双链路 → 引用卡片
- 为什么 AI 应用岗的主流是 TS 全栈（以及与 Go 后端的岗位分工）
- Vercel AI SDK 流式协议：UI Message Stream、text part 与 data part、前端 `useChat`
- mock 模式：无 API key 也能端到端联调的可测试性设计
- 代码精讲：TS 版 RAG 底座与 Go 侧的同构对照、ingest/chat 两条链路逐段讲
- 进阶：引用卡片的协议设计、手写 SSE 最小实现、生产化清单
- 面试：口述项目架构（30 秒版 + 5 分钟版）、AI SDK 的价值边界、流式 UI 取舍
- 动手练习：阶段二练习 6（ingest pipeline）、练习 7（问答 UI + 引用卡片）

---

## 一、概念详解

### 1.1 项目 2 全景架构：把 RAG 从 CLI 搬进浏览器

第 5 章你在 Go CLI 里跑通了 RAG：灌文档 → 切块 → 入库 → 检索 → 带引用回答。项目 2 做的是**同一条链路的"产品化"**：CLI 换成浏览器界面，一次性输出换成流式打字机，文字引用换成可点击展开的引用卡片。架构全景：

```
浏览器（React）
  app/page.tsx：useChat 管理对话状态，渲染消息流 + 引用卡片
     │  ① POST /api/chat    { messages: UIMessage[] }（全量历史，每轮重发）
     │  ② POST /api/ingest  multipart 表单（字段名 "file"）
     ▼
Next.js Route Handlers（同一个 Node 进程内）
  ┌─ app/api/ingest/route.ts ── 写入路径（离线）
  │    解析上传 → chunk（lib/chunk.ts）→ embedTexts（lib/embed.ts）
  │    → store.add（lib/vectorstore.ts）→ 落盘 data/kb.json（lib/kb.ts）
  └─ app/api/chat/route.ts ──── 查询路径（在线）
       取最后一轮 query → embed(query) → store.search top-k
       → 检索块拼进 system prompt（带编号、要求标注来源）
       → streamText 流式生成 → SSE 流回前端：
         先 data part（引用来源），后 text part（回答正文）
     ▼
外部服务：硅基流动（embedding，BAAI/bge-m3，1024 维）
          DeepSeek（生成，deepseek-chat，OpenAI 兼容协议）
```

记住三个架构事实，后面所有细节都挂在它们上面：

1. **两条路径共享同一个进程内向量库**。ingest 入库的文档，chat 要能立刻检索到，所以 `Store` 必须是进程级单例（`lib/kb.ts`）——这也决定了这个架构的边界（见进阶 3.3 的 serverless 讨论）。
2. **服务端无会话状态**。和第 1 章"历史即状态"完全一致：前端每轮把全量 messages 发过来，服务端不存对话。它只存一样东西——知识库（向量库），那是"知识"不是"会话"。
3. **引用来源不走模型，走协议**。检索到的资料块，一份塞进 system prompt 给模型看（生成时用），一份作为 data part 直接随流推给前端（渲染时用）。模型只负责"写带 [N] 编号的回答"，编号的真相由服务端保管——这是 1.3 节和进阶 3.1 的核心。

> **读代码前的三个 Next.js 约定**（没碰过 Next.js 也能跟上本章）：
>
> 1. **文件路径即路由**：`app/api/ingest/route.ts` 就是 `/api/ingest`，`app/page.tsx` 就是 `/`——没有单独的路由配置文件；
> 2. **导出函数名即 HTTP 方法**：Route Handler 里 `export async function POST(req)` 处理 POST 请求，换成 `GET` 同理；
> 3. **`"use client"` 是服务端/客户端的分界线**：App Router 组件默认在服务端渲染（不能 `useState`、不能交互），文件顶部写 `"use client"` 才下放到浏览器执行（`app/page.tsx`）；Route Handler 永远在服务端——所以它能安全读 `process.env` 里的密钥（常见坑 2 会展开）。

### 1.2 为什么 AI 应用岗的主流是 TS 全栈

写过 Go CLI 之后值得停下来想一个问题：为什么这类 AI 应用岗位，JD 里高频出现的是 Next.js + Vercel AI SDK 这套 TS 组合？

- **产品形态决定技术选型**。AI 应用的核心体验在交互层：流式逐字渲染、工具调用过程可视化、引用卡片、生成中途的编辑与重试。这些体验和"流式协议解析 + 前端状态机"强耦合。TS 一套语言贯通浏览器与服务端，AI SDK 把这套耦合打包成 `useChat` 一个 hook——产品迭代速度是这套栈的真正卖点。
- **生态事实**。Vercel AI SDK 是目前 TS 生态里覆盖最广的 LLM 接入层：统一了各家模型厂商的协议差异（本项目 `ai@7`、`@ai-sdk/openai@4`，以 `package.json` 为准），流式协议、工具调用、结构化输出都有现成抽象。
- **与 Go 后端的岗位分工**。面向用户的接入层、快速产品化偏 TS 全栈；重编排（多 Agent 并发）、高 QPS、基础设施偏 Go。本教程第 8-13 章的项目 3 就是这个分工的实例：Go 写多 Agent 编排引擎，TS 写实时看板。两条线在招聘市场都真实存在，"全栈 AI 工程师"两个都要会——所以本教程把两线都走一遍。

本章还有一个隐藏教学点：**第 4-5 章你用 Go 手写了 embedding 客户端、向量库、chunking；本章的 `lib/` 是同一套东西的 TS 版**。面试时讲"同一套 RAG 底座我用两种语言实现过，能讲清每处语言差异"——这比"我调过 xx SDK"有力得多。代码精讲 2.1 节专门做这份对照。

### 1.3 Vercel AI SDK 流式协议：一条 SSE 流里跑两类"零件"

AI SDK 的流式响应建立在 SSE（Server-Sent Events）上。第 3 章你在 Go 侧手写过 SSE 解析（`bufio` 逐行扫 `data: ` 前缀、`[DONE]` 收尾），本章在浏览器侧消费**同一种协议**——SSE 是语言中立的文本协议，这是它成为 LLM 流式事实标准的原因。

AI SDK 在 SSE 之上定义了自己的帧格式——**UI Message Stream**：每个 SSE `data:` 行的载荷是一个 JSON 对象，叫一个 **part**；一条助手消息由一串 part 拼成。直接从本项目的依赖实现里可以看到线格式（`node_modules/ai/dist/index.js`，ai@7.0.52）：每个 part 序列化为 `data: {json}\n\n`，流尾追加 `data: [DONE]\n\n`，响应头带 `content-type: text/event-stream` 与 `x-vercel-ai-ui-message-stream: v1`。一条典型的流长这样：

```text
data: {"type":"data-sources","data":[{"id":"guide.md#0","source":"guide.md",...}]}

data: {"type":"text-start","id":"text-1"}

data: {"type":"text-delta","id":"text-1","delta":"根据资料"}

data: {"type":"text-delta","id":"text-1","delta":" [1]，RAG 是"}

data: {"type":"text-end","id":"text-1"}

data: [DONE]
```

part 分两大类，这个区分是全章重点：

- **text part**（`text-start` / `text-delta` / `text-end`）：模型生成的正文，逐块增量到达，前端拼接渲染出打字机效果；
- **data part**（`data-*`）：服务端主动写入的结构化数据——本项目的引用来源就是 `data-sources` part。它不是模型生成的，是服务端代码"夹带"进同一条流的。

涉及的 API 只有三个，先看全景再去代码里对号：

- 服务端 `createUIMessageStream`（`app/api/chat/route.ts:275`）：给你一个 `writer`，既能 `writer.write` 自定义 data part，又能 `writer.merge` 把 `streamText` 的模型流并进同一条 SSE；
- 服务端 `createUIMessageStreamResponse`（`route.ts:298`）：把这条流包装成带正确响应头的 HTTP 响应；
- 前端 `useChat`（`app/page.tsx:28`）：POST 全量 messages、解析 SSE、把到达的 part 逐个拼进 `message.parts`，驱动 React 重渲染。

一个版本措辞提醒：早期资料里的 `createDataStream` / Data Stream Protocol 是 AI SDK 上一代 API；本项目用的 ai v7 对应物是 UI Message Stream（`createUIMessageStream` / `createUIMessageStreamResponse`），概念相同、帧格式更规整。面试时两个名字都可能被提到，知道对应关系即可，细节以你所用版本的官方文档为准。

最后是一对容易混淆的类型：

| 类型 | 在哪用 | 形状 |
| --- | --- | --- |
| `UIMessage` | 前端 ↔ 服务端之间的协议格式 | 文本在 `parts` 数组里（混着 data part 等其他类型） |
| `ModelMessage` | 服务端 → 模型 API 的格式 | 第 1 章的 role/content 结构 |

服务端拿到前端发来的 `UIMessage[]` 后，要用 `convertToModelMessages` 转成模型消息才能喂给 LLM（`route.ts:286`，注意本项目所用版本里它是异步的，返回 Promise）。

### 1.4 mock 模式：没有 API key 也能端到端联调

项目 2 依赖两个外部服务（硅基流动 embedding、DeepSeek 生成）。没 key、没网、怕花钱时怎么开发？项目的答案是两个环境变量开关：

- `EMBEDDING_MOCK=1`：`embedTexts` 返回**确定性假向量**——同一段文本永远得到同一个向量（`lib/embed.ts:77-79`，实现见 2.1 节）；
- `CHAT_MOCK=1`：聊天模型换成 AI SDK 自带的 `MockLanguageModelV3`，返回罐装流式回答（`app/api/chat/route.ts:94-135`）。

设计精髓在于 **mock 只替换最外层依赖那一层**：`CHAT_MOCK` 换掉的是模型，但检索管线、SSE 协议、data part、前端渲染全部走真实代码路径；`EMBEDDING_MOCK` 换掉的是 embedding API，但 chunking、入库、落盘、检索全部是真的。所以"mock 模式联调通过 ≈ 真实模式只剩换 key"。

这正是测试替身（test double / mock-stub）思想在手工联调场景的应用，也是一个可复用的工程习惯：**给每个外部依赖留一个确定性 fake，你的开发节奏就不再被网络和账号余额绑架**。假向量没有语义（检索质量无意义），但"管线通不通"能完整验证。

---

## 二、代码精讲

### 2.1 TS 版 RAG 底座三件套：与 Go 侧的同构对照

`stage-02-kb-agent/lib/` 的三个文件，与你在 `mini-agent/` 里完成的 Go 练习一一对应：

| TS（本项目） | Go（mini-agent） | 职责 |
| --- | --- | --- |
| `lib/embed.ts` `embedTexts`（:66） | `internal/embed/embed.go` `Client.Embed`（:93） | 文本 → 向量，批量调用，按 index 归位 |
| `lib/chunk.ts` `chunk`（:69） | `internal/rag/chunk.go` `Chunk`（:78） | 结构优先切分 + 固定窗口 overlap 兜底 |
| `lib/vectorstore.ts` `Store`（:99） | `internal/vectorstore/store.go` `Store`（:59） | 余弦相似度 + 暴力 top-k + JSON 持久化 |

策略层面两边**逐行同构**（TS 版头注释明确说是 Go 侧参考答案的移植），真正值得讲的是语言差异——`lib/vectorstore.ts:12-17` 的头注释已经替你总结了三条，这就是面试素材：

1. Go 的 `[]float32` 在 TS 里没有对应类型，统一用 `number`（float64）；JSON 序列化因此天然"最短可往返"，比 Go 侧省心；
2. Go 排序要显式 `sort.SliceStable` 才稳定；JS 的 `Array.prototype.sort` 自 ES2019 起规范保证稳定排序（`vectorstore.ts:169-171`）；
3. Go 用 error 返回值，TS 用 throw——对"调用方 bug"（维度不符、topK≤0、零向量）一律 fail fast，因为静默继续会把错误伪装成"检索质量差"。

逐文件看关键实现。

**`lib/embed.ts`**——`embedTexts`（:66）的骨架五步：入参校验 → mock 分支 → 取 key → fetch → 按 index 归位。两处必须停下的细节：

```ts
// embed.ts:97-99 —— 非 200 把状态码和响应体带进错误信息：
// 排查限流/余额问题全靠它。裸的 "fetch failed" 是排障噩梦。
if (!resp.ok) {
  throw new Error(`embed: HTTP ${resp.status}: ${await resp.text()}`);
}
```

```ts
// embed.ts:112-127 —— 全模块最核心的坑（与 Go 侧完全相同）：
// 响应 data 数组的顺序不能假设与输入一致！每个元素带 index 字段
// 标明对应 input 的第几段，归位必须按 index 放。
const result: number[][] = new Array(texts.length);
for (const d of data) {
  if (d.index < 0 || d.index >= texts.length) { /* 越界报错 */ }
  if (d.embedding.length !== BGE_M3_DIMENSIONS) { /* 维度报错 */ }
  result[d.index] = d.embedding;
}
// :129-133 再扫一遍空洞——防止服务端漏返回导致"静默错位"
```

维度校验（`embed.ts:20` 写死 1024）的意义：入库前发现"模型/服务商配错"，否则错误向量悄悄入库，检索结果全错还极难排查。另外注意写入路径和查询路径共用本模块，**两侧必须用同一个 embedding 模型，向量空间才对齐**——这是 RAG 最隐蔽的 bug 来源之一。

mock 路径 `mockEmbedding`（`embed.ts:138-158`）也值得读：FNV 风格 hash 把文本变成种子，再用 xorshift 伪随机发生器展开成 1024 维向量。两处设计意图——**确定性**（同文本同向量，否则检索连"管线通不通"都验证不了）和**正确维度**（下游维度校验照常工作）。

**`lib/chunk.ts`**——`chunk`（:69-101）是"结构优先，窗口兜底"：先按 `\n\n` 切段落（`splitParagraphs`，:103），贪心地把整段打包进块（段落不拆散，分隔符 `\n\n` 也占额度）；单段自身超限时才对该段做固定窗口硬切（`hardCut`，:110-122，步长 = maxChars − overlapChars）。`normalizeChunkOptions`（:124-138）把 overlap 钳制到 `[0, maxChars-1]`——否则步长为 0 或负数，硬切循环永不前进（死循环）。

TS 版独有的坑在 `codePoints`（:66）：

```ts
const codePoints = (s: string): string[] => Array.from(s);
```

JS 字符串按 UTF-16 code unit 索引，`str.length` / `slice()` 都以 code unit 为单位。中文 BMP 字符恰好 1 个 code unit 看起来安全，但 emoji 和很多生僻字是代理对（2 个 code unit），按 code unit 切会把一个 emoji 劈成两个非法"半个字符"。切分和计量都基于码点数组——**这和 Go 侧"按 rune 不按 byte"是同一个坑在不同语言的形态**。

**`lib/vectorstore.ts`**——三个方法对应三条纪律：

- `cosineSimilarity`（:72-91）：维度不等报错、空向量报错、**零向量报错**（模长为 0 没有"方向"，相似度无定义；若静默产出 NaN，`sort` 比较函数对 NaN 恒返回 false，排序结果不可预期且不报任何错）；
- `add`（:114-138）：**all-or-nothing**——先整批校验再统一追加，任一文档失败整批拒绝，调用方重试时不用先清理中间状态；全库维度一致是硬约束（维度混杂的真实来源通常是"换了 embedding 模型忘了重建索引"）；
- `save` / `load`（:191-243）：**原子写入**——先写同目录临时文件再 `rename` 覆盖（rename 在同文件系统内是原子操作，任意时刻目标路径要么完整旧版要么完整新版）；`load` 先校验到局部变量再整体替换，"重载一个坏文件"不会冲掉正在运行的库。持久化的动机：内存库进程退出即丢，重建索引要重新调 embedding API——既花钱又慢，所以入库一次、落盘复用。

为什么手写暴力检索而不是直接用向量数据库？文件头注释（:19-24）给了面试反直觉考点：1024 维向量一次点积就是 1024 次乘加，10 万条记录约 1 亿次浮点运算，现代 CPU 毫秒级跑完——"必须上 ANN（HNSW）"只在百万级以上才成立，且 ANN 是近似、会丢召回。

### 2.2 进程级单例：`lib/kb.ts`

写入路径和查询路径要共享同一个 `Store` 实例，但直接写模块级变量在 Next.js 里有个坑：**dev 模式热重载会重新执行模块代码**，模块级变量被重置，库就"丢"了。解法（`lib/kb.ts:25-41`）：

```ts
const globalForKb = globalThis as unknown as { __kbStore?: Store };

export function getKbStore(): Store {
  if (!globalForKb.__kbStore) {
    const store = new Store();
    if (existsSync(KB_PATH)) {
      store.load(KB_PATH); // 进程重启不丢索引：启动时从落盘文件恢复
    }
    globalForKb.__kbStore = store;
  }
  return globalForKb.__kbStore;
}
```

`globalThis` 在整个 Node 进程里只有一份，热重载后还在——这是 Next.js 项目保存进程级状态的惯用法（Prisma 官方示例同款）。同时要记住边界（`kb.ts:12-13` 头注释）：**生产环境多实例 / serverless 下这个模式不成立**，每个实例各有一份内存库，得换外部存储。这正是"内存向量库"架构的天花板，进阶 3.3 展开。

### 2.3 写入路径：`app/api/ingest/route.ts`

`POST`（:25）一次请求走完"上传 → 解析 → chunking → embedding → 入库 → 落盘 → 返回统计"全链路。前半段是安全边界（Route Handler 是公网入口，必须假设输入是恶意的）：

- **multipart 解析**（:32-40）：Web 标准 `req.formData()`，不需要 multer 这类库。非 multipart 请求会让 `formData()` 直接抛异常——不兜住的话客户端只看到一个没有任何错误信息的裸 500；
- **字段与类型白名单**（:41-61）：只认字段名 `"file"`；扩展名放行 `.md` / `.txt` / `.pdf`（PDF 用 `unpdf` 提取文本，:69-89，这是练习 6 的进阶项）；
- **大小限制**（:62-67）：`MAX_FILE_BYTES = 5MB`（:23），防超大文件打爆内存 / 刷 embedding 费用。

后半段是练习 6 的组装逻辑（:132-163），五步：

```ts
const chunks = chunk(text);                 // ① 切分（:132）
const vectors = await embedTexts(chunks);   // ② 批量 embedding（:138）
const docs: Document[] = chunks.map((t, i) => ({   // ③ 组装记录（:140-148）
  id: `${file.name}#${i}`,                  //    可读、可定位回来源块
  text: t,
  vector: vectors[i],
  metadata: { source: file.name, chunk: String(i) }, // 练习 7 的引用全靠它
}));
store.add(...docs);                          // ④ 入库（all-or-nothing，:150）
store.save(KB_PATH);                         // ⑤ 落盘（:151）
```

`metadata` 这两个字段是全章的"暗线"：**引用溯源（"这个答案来自《XX 文档》第 3 块"）全靠入库时写下的元数据**。没有它，后面查询路径的引用卡片就是无源之水。另外注意 `store.add` 是 all-or-nothing，维度不符会整批 throw，外层 try/catch（:158-163）兜住返回 500 + 错误信息——入库失败的一致性问题，面试 Q5 专门展开。

### 2.4 引用数据协议：`lib/chat-types.ts`

这个只有 55 行的文件定义了前后端共享的数据协议，是"引用卡片"的地基：

```ts
// chat-types.ts:20-31 —— 一条引用来源的最小可溯源集合
export interface SourceItem {
  id: string;      // 向量库 id（如 "guide.md#3"），同时是前端渲染的 key
  source: string;  // 来源文件名（metadata.source）
  chunk: string;   // 块序号（metadata.chunk）
  text: string;    // 块全文：卡片折叠时只显示预览，点击展开看的就是它
  score: number;   // 余弦相似度得分，帮助用户判断相关性强弱
}
```

为什么单独抽一个文件？服务端（`route.ts`）和客户端（`page.tsx`、`sources-card.tsx`）都要认识"引用来源"的结构，各自定义一份会漂移——抽出来保证"服务端写出去的"和"前端读进来的"是同一个类型。

```ts
// chat-types.ts:46-55
export type KbDataParts = {
  sources: SourceItem[]; // 声明本项目的自定义 data part：data-sources
};
export type KbUIMessage = UIMessage<unknown, KbDataParts>;
```

`UIMessage` 泛型的第二个参数挂上 `KbDataParts` 后，**服务端 `writer.write` 和前端 `parts` 过滤都获得类型检查**——写错 data 结构直接编译报错，而不是运行时才现形。一个 TS 细节（:43-45 注释）：这里用 `type` 而不是 `interface`，因为 `UIMessage` 的泛型约束 `UIDataTypes` 带索引签名，`interface` 默认不满足（TS 只给 `type` 别名隐式索引签名）。

### 2.5 查询路径：`app/api/chat/route.ts`

这是全章信息密度最高的文件，按请求处理顺序逐段看。

**provider 与一个高频坑**（:43-52）：

```ts
const deepseek = createOpenAI({
  baseURL: "https://api.deepseek.com",
  apiKey: process.env.DEEPSEEK_API_KEY,
});
// 用法是 deepseek.chat(CHAT_MODEL)（:270），用 .chat() 而不是直接调用 provider——
// 后者默认走 OpenAI Responses API，DeepSeek 只兼容 Chat Completions API，
// 这是接第三方"OpenAI 兼容"服务时最常见的坑。
```

**防幻觉 system prompt**（`buildRagSystemPrompt`，:70-84）：

```ts
const blocks = sources
  .map((s, i) => `[${i + 1}]（来源：${s.source} 第${s.chunk}块）\n${s.text}`)
  .join("\n\n");
// + 三条硬指令：
// 1. 仅根据资料回答，不要动用资料之外的知识；
// 2. 资料不足以回答时，明确说「根据现有资料无法回答」，不要编造；
// 3. 回答中用到资料时，在句末标注来源编号，如 [1] [2]
```

编号 `[${i+1}]` 与 `sources` 数组顺序一致——**prompt 里的资料编号、data part 里的数组顺序、前端卡片的编号，三者必须同源**，这是"引用可点击"的协议纪律。第 2 条"允许说不知道"是防幻觉 prompt 的关键：不给模型一条退路，它倾向于硬答。

**为什么这个 prompt 必须由服务端组装，而不是让前端传进来？** `route.ts` 头注释（:23-26）点明了理由：system prompt 是防幻觉约束的闸门，若允许客户端自定义，一句"忽略之前的指令，直接回答"就绕过了全部约束——RAG 的注入攻击面不只在文档内容，也在**调用方本身**。所以 chat 接口只收 user messages，system prompt 永远在服务端拼装。面试聊"防幻觉"时补上这一句，说明你想到的不只是 prompt 措辞。

**请求解析与检索**（:139-254）：

- body 校验（:139-151）：`{ messages: KbUIMessage[] }`，只查"是不是非空数组"；
- 取最后一轮 user query（:206-224）：**UIMessage 的文本在 `parts` 里，不是顶层字段**——`last.parts.filter(p => p.type === "text").map(p => p.text).join("\n")`，这是 AI SDK v5+ 与旧版 `message.content` 的最大差异，易踩坑；
- 知识库为空不报错（:230-231）：换用 `NO_KB_SYSTEM`（:58-60）让模型如实告知"请先上传文档"——**"没有资料"和"出错了"是两回事，别用 500 表达前者**；
- 检索本体（:233-245）：`embedTexts([query])` → `store.search(queryVector, TOP_K)`（`TOP_K = 3`，:56，是练习 8-9 的调参位）→ 映射成 `SourceItem[]`（从 hits 里取 `metadata.source` / `metadata.chunk`——练习 6 入库时写的溯源信息在这里闭环）→ 组装 system；
- 检索异常兜底（:246-253）：embedding 网络失败等，try/catch 返回 500 + 错误信息，别让客户端看到无信息的裸错误。

**流式管线**（:275-298），全章的高潮：

```ts
const stream = createUIMessageStream<KbUIMessage>({
  execute: async ({ writer }) => {
    // ① 引用来源先于文本到达前端：data part 出现在助手消息的 parts 里
    writer.write({ type: "data-sources", data: sources });
    const result = streamText({
      model,
      system,
      // ② UIMessage（前端格式）→ ModelMessage（模型格式）；
      //    全量历史每轮都发给模型——LLM 本身无记忆，"对话"靠重放历史
      messages: await convertToModelMessages(messages),
    });
    writer.merge(result.toUIMessageStream()); // ③ 模型流并进同一条 SSE
  },
  onError: (err) => {
    console.error("chat stream error:", err);
    return "生成回答时出错，请查看服务端日志"; // ④ 不透内部细节给客户端
  },
});
return createUIMessageStreamResponse({ stream });
```

四个设计点：

1. **引用来源走 data part 而不是塞进回答文本**（文件头注释 :18-21）：来源是结构化数据，让模型把来源"说"出来既浪费 token 又不可控（模型可能漏报、错报编号）；服务端直接随流推给前端，前端拿到类型化数据，想怎么渲染都行；
2. `convertToModelMessages` 是异步的（本项目 ai v7 起返回 Promise），别漏了 `await`；
3. `writer.merge` 把模型流并入——同一条 SSE 流里先有 `data-sources` 再有 `text-*`，前端因此能做出"引用先出"的效果；
4. `onError` 不把服务端错误细节吐给客户端（可能含 key / 内部路径），统一返回一句话，细节留在服务端日志——流式中途的错误处理，常见坑第 3 条展开。

**mock 模型**（`mockChatModel`，:94-135）：用 `MockLanguageModelV3` + `simulateReadableStream` 把罐装文本按约 12 字切片、每块间隔 60ms 推送，模拟真实模型的打字机效果。注意罐装回答故意带 `[1]` 编号（:66-68）——离线也能验证"回答标注来源编号"的渲染效果。

### 2.6 前端：`app/page.tsx` 与 `components/sources-card.tsx`

`page.tsx` 的核心是一个 hook（:28-30）：

```ts
const { messages, sendMessage, status, error } = useChat<KbUIMessage>({
  transport: new DefaultChatTransport({ api: "/api/chat" }),
});
```

`useChat` 替你做的事（文件头注释 :5-11）：维护 messages 状态机（`status: submitted → streaming → ready / error`，:32 用它禁用输入）；`sendMessage` 时把全量 messages POST 到 `/api/chat`；解析 SSE，把 `text-delta` 增量拼进助手消息的 parts——React 状态每收一块就更新，于是呈现打字机效果。**流式渲染的本质不是特殊技巧，就是"高频 setState"**。

渲染侧（:62-87）两个细节：文本要从 `m.parts` 里按 `part.type === "text"` 过滤出来（:75-81，parts 里可能混着 data part）；助手消息下方挂 `<SourcesCard message={m} />`（:83）。

`sources-card.tsx` 是练习 7② 的实现位置，逻辑四步：

1. 从 `message.parts` 里找 `data-sources` part（:56-59），没有或为空就不渲染——`KbUIMessage` 泛型保证找到后 `part.data` 就是 `SourceItem[]`；
2. 编号按**数组顺序**从 1 开始（:89-104），不按 score 重排——必须与服务端 system prompt 里的资料编号一致（2.5 节的协议纪律）；
3. 点击卡片展开/收起全文：`useState<Set<number>>` 记录展开状态（:54、:63-73）；
4. 预览截取前 80 字用 `Array.from(s.text)` 按码点切（:83-87）——和 chunk 的 UTF-16 坑同源。

另外注意头注释 :5-7 点出了这个组件的产品价值：引用 = 可验证性 = 用户信任 + 幻觉兜底。用户能点开原文核对"模型是不是照资料答的"，这是 RAG 相对纯 LLM 问答的核心卖点。

---

## 三、进阶拓展（带代码）

### 3.1 引用卡片的流式协议设计：text part 与 data part 怎么分工

**为什么这样设计**：一条助手的"回复"其实包含两类信息——给**人**看的生成文本，和给**代码**消费的结构化事实。硬把它们混在一条文本流里，三个问题接踵而至：模型复述来源浪费 token；模型可能漏报、错报编号（生成内容不可控）；前端想做卡片、折叠、跳转就得从文本里抠结构（脆弱的正则）。AI SDK 的答案是在同一条 SSE 流里区分 part 类型：

- **text part**：模型生成的内容，逐 delta 到达；
- **data part**：服务端掌握的事实，由服务端代码主动 `writer.write`，类型固定为 `data-${key}`。

分工原则一句话：**模型"说"的走 text，服务端"知道"的走 data**。

**教学示例**：假设你想在产品里显示"本次检索耗时 / 命中分数"，给前端调试面板用。加一个自定义 data part 只需三步，且全程有类型检查：

```ts
// ① lib/chat-types.ts —— 在 KbDataParts 里声明新 part（教学示例）
export type KbDataParts = {
  sources: SourceItem[];
  retrievalStats: { topK: number; latencyMs: number; bestScore: number };
};

// ② app/api/chat/route.ts —— 检索完成后写入流（教学示例）
const t0 = Date.now();
const hits = store.search(queryVector, TOP_K);
writer.write({
  type: "data-retrievalStats",
  data: { topK: TOP_K, latencyMs: Date.now() - t0, bestScore: hits[0]?.score ?? 0 },
});

// ③ 前端组件 —— 按类型过滤出来渲染（教学示例）
const stats = message.parts.find((p) => p.type === "data-retrievalStats");
if (stats) {
  // stats.data 的类型就是 { topK, latencyMs, bestScore }，写错字段编译期报错
  console.log(`检索 ${stats.data.topK} 条，耗时 ${stats.data.latencyMs}ms`);
}
```

**取舍与生产注意**：

- **体积**：data part 会随 `UIMessage.parts` 留在对话历史里，`useChat` 默认每轮把全量历史回传服务端——引用全文较大时，历史里会累积多份 sources。本项目学习场景可接受；生产可考虑只保留最新一条、或对 `text` 字段做截断（具体行为以你所用版本的官方文档为准）。
- **顺序**：先写 data 再 merge 模型流，前端"引用先出"；反过来用户会先看到答案、半天后才冒出卡片，体验割裂。
- **不要把 data part 当隐藏信道塞敏感数据**：它对前端完全可见，和文本一样会进浏览器的内存与日志。

### 3.2 手写 SSE 替代 AI SDK 的最小实现——理解 SDK 到底帮你做了什么

**为什么值得手写一遍**：面试里"AI SDK 帮你做了什么"的好答案，前提是你说得出没有它时要自己写哪些代码。下面是一个自洽的最小实现（教学示例）：Next.js route 手工 `ReadableStream` 推 `data:` 行，浏览器用 `fetch` 流式读取、手工拼帧解析。

服务端（`app/api/manual-stream/route.ts`，教学示例）：

```ts
// 手写 SSE 服务端：不依赖 AI SDK，演示线格式本身。
export async function POST(req: Request) {
  const { q } = (await req.json()) as { q?: string };
  const answer = `你问的是「${q ?? "?"}」。这是手写 SSE 的演示回答，逐块推送。`;
  const encoder = new TextEncoder();

  const stream = new ReadableStream<Uint8Array>({
    async start(controller) {
      // 每个事件 = 一行 "data: " + JSON + 空行（SSE 帧以 \n\n 结尾）
      const send = (obj: unknown) =>
        controller.enqueue(encoder.encode(`data: ${JSON.stringify(obj)}\n\n`));
      try {
        send({ type: "meta", source: "manual-sse-demo" }); // 自定义"数据帧"
        for (let i = 0; i < answer.length; i += 4) {
          send({ type: "delta", text: answer.slice(i, i + 4) }); // 文本增量帧
          await new Promise((r) => setTimeout(r, 80)); // 模拟模型逐 token 输出
        }
        controller.enqueue(encoder.encode("data: [DONE]\n\n")); // 约定终结符
        controller.close();
      } catch (err) {
        // 流已开始，HTTP 状态码发不出去了——错误只能作为"错误帧"随流下发
        send({ type: "error", message: String(err) });
        controller.close();
      }
    },
  });

  return new Response(stream, {
    headers: {
      "content-type": "text/event-stream",
      "cache-control": "no-cache",
      connection: "keep-alive",
      // 反代（nginx）默认会缓冲响应，流式必须显式关掉
      "x-accel-buffering": "no",
    },
  });
}
```

客户端（浏览器侧，教学示例）：

```ts
// 手写 SSE 客户端：fetch 流式读取 + 手工拼帧。
// 注意不能用 EventSource——它只支持 GET，而聊天接口是 POST。
async function consumeSSE(url: string, body: unknown): Promise<string> {
  const resp = await fetch(url, {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: JSON.stringify(body),
  });
  if (!resp.ok || !resp.body) throw new Error(`HTTP ${resp.status}`);

  const reader = resp.body.getReader();
  const decoder = new TextDecoder();
  let buffer = "";
  let answer = "";

  for (;;) {
    const { done, value } = await reader.read();
    if (done) break;
    buffer += decoder.decode(value, { stream: true });

    // 核心难点（与第 3 章 Go 侧完全相同）：网络分块可能把一个帧切开，
    // 必须按空行拼帧——buffer 里不够一个完整帧就等下一块。
    let sep: number;
    while ((sep = buffer.indexOf("\n\n")) !== -1) {
      const frame = buffer.slice(0, sep);
      buffer = buffer.slice(sep + 2);
      for (const line of frame.split("\n")) {
        if (!line.startsWith("data: ")) continue; // 忽略注释/心跳等其他行
        const payload = line.slice("data: ".length);
        if (payload === "[DONE]") return answer;
        const part = JSON.parse(payload) as { type: string; text?: string };
        if (part.type === "delta" && part.text) answer += part.text;
      }
    }
  }
  return answer;
}
```

手写一遍后，SDK 的价值清单就具象了——它在你写的这些代码之上还提供了：part 类型协议（`text-start/delta/end` 配对与 id 管理）、消息生命周期状态机（`submitted/streaming/ready/error`）、data part 的泛型类型化、错误帧约定、React 状态集成（`useChat`）、断流恢复等企业级细节。手写要处理的难点（拼帧、缓冲、终结符、错误帧）和第 3 章 Go 侧 `bufio` 解析完全同构——**同一协议两种语言，第三次见就是本能**。

生产注意：手写路线还要自己处理反代缓冲（上面的 `x-accel-buffering`）、连接被浏览器/中间盒提前关闭、客户端无自动重连等问题——这正是多数团队最终回到 SDK 的原因。

### 3.3 生产化清单：从学习项目到能上线

本项目是学习定位，但每处"从简"都要知道生产上补什么：

**密钥管理（服务端-only env）**。三个 key（`DEEPSEEK_API_KEY` / `SILICONFLOW_API_KEY` / mock 开关）都只出现在 Route Handler 的 `process.env` 里，绝不下发到浏览器。Next.js 的语义防线是命名：**带 `NEXT_PUBLIC_` 前缀的环境变量会被内联进客户端 bundle**，任何用户都能从 devtools 里翻到——密钥永远不要加这个前缀。常见坑第 2 条有事故形态。配套纪律：`onError` 不回传内部错误细节（`route.ts:292-295` 已是范例）。

**上传文件的大小/类型限制**。本项目已有：5MB 上限（`route.ts:23`）、扩展名白名单（:49-61）、PDF 解析失败兜底（:79-86）。生产还要加：鉴权（谁能上传）、限流（防刷 embedding 费用）、内容审计（知识库会被检索进 prompt，恶意文档 = 间接 prompt 注入的入口，呼应第 1 章进阶 3.1）。

**并发入库的 embedding 批量策略**。本项目一次请求把全部 chunk 打一个批量 POST（`embed.ts:94`）——学习项目够用，但服务商对单批数量有限制（bge-m3 单条输入上限 8192 token，具体以硅基流动官方文档为准），大文档必须分批。分批 + 有限并发的教学实现：

```ts
// embedInBatches：大文档入库时的分批 + 有限并发封装（教学示例，基于 lib/embed.ts）。
import { embedTexts } from "@/lib/embed";

export async function embedInBatches(
  texts: string[],
  batchSize = 32,
  concurrency = 2,
): Promise<number[][]> {
  const result: number[][] = new Array(texts.length);
  let next = 0; // 下一个待认领的批次起点

  async function worker() {
    while (next < texts.length) {
      const start = next;
      next += batchSize; // 认领批次：JS 单线程，读与自增之间无 await，无需加锁
      const slice = texts.slice(start, start + batchSize);
      if (slice.length === 0) return;
      const vectors = await embedTexts(slice); // 复用单批实现（含 index 归位）
      for (let i = 0; i < vectors.length; i++) {
        result[start + i] = vectors[i]; // 按全局下标归位，批次间互不干扰
      }
    }
  }

  await Promise.all(Array.from({ length: concurrency }, worker));
  return result;
}
```

并发度别开大：embedding 服务商一样有速率限制，超了就是 429。更省的做法是入库前按内容 hash 去重（同一块不重复 embed）。

**部署到 Vercel 的注意点**——每一条都对应本项目架构的一个假设失效：

- **文件系统是临时的**：`data/kb.json` 落盘模式（`kb.ts:23`）在 serverless 上不持久，换外部向量库（pgvector / Qdrant / Pinecone，选型见第 4 章）；
- **多实例不共享内存**：`globalThis` 单例（`kb.ts:25`）只在单进程内成立，serverless 每个冷启动实例各一份——内存库方案整体让位于外部存储；
- **函数有执行时长上限**：流式响应期间函数必须存活，注意平台 `maxDuration` 配置；embedding 批量调用要卡进超时预算；
- **env 在平台控制台配置**，不要提交 `.env.local` 进仓库（本项目 `.env.example` 只留键名是正确示范）。

---

## 四、面试视角

> 以下每题给"标准回答 → 常见追问 → 加分点"。自测方法：不看回答口述，再对照差距。

**Q1：口述一下你做的知识库 Agent 项目架构。**

标准回答（30 秒电梯版）："一个 Next.js 全栈 RAG 应用。写入路径：用户上传 md/txt/PDF，服务端 chunking（结构优先 + 窗口兜底）、批量 embedding（bge-m3），存入内存向量库并 JSON 落盘。查询路径：用户问题先 embed 取向量，top-k 检索，检索块带编号拼进防幻觉 system prompt，DeepSeek 流式生成；回答文本和引用来源通过 AI SDK 的 UI Message Stream 走同一条 SSE 流回前端——文本逐字渲染，引用渲染成可点击展开的卡片。"

标准回答（5 分钟架构版）：在 30 秒版基础上按本章节拍展开——双链路图（1.1 节）→ 三个关键设计（引用走 data part 不走文本；`globalThis` 单例抗 dev 热重载及其 serverless 边界；`EMBEDDING_MOCK` / `CHAT_MOCK` 可测试性设计）→ 三条工程纪律（embedding 按 index 归位、入库 all-or-nothing、落盘原子写）→ 已知边界与生产化方向（内存库换外部向量库、鉴权限流、分批并发）。这道题就是第 14 章系统设计题"企业知识库问答"的预演，讲的时候主动画双链路图。

追问链：
- "知识库更新了怎么办？" → 重新上传即追加；但重复上传会产生重复块（`ingest/route.ts` TODO 里留的设计讨论点）：按 `metadata.source` 过滤旧块再入库，或删除重建。生产做法是文档级版本管理 + 幂等键；
- "多人同时用会怎样？" → Node 单线程内单例读写无数据竞争，读写都很快；但多实例部署就各自一份库、数据不一致——这是内存架构的边界，解法是外部向量库；
- "为什么不在浏览器里直接调 embedding API？" → API key 会随客户端 bundle 泄漏（常见坑第 2 条），且密钥、配额、重试都应该服务端集中管控。Route Handler 就是 BFF 层。

**Q2：AI SDK 帮你做了什么？如果不用它，你要手写哪些东西？**

标准回答：四层——① 模型厂商协议适配（`createOpenAI` 换 baseURL 接 DeepSeek）；② UI Message Stream 协议（part 类型、SSE 帧序列化、响应头）；③ 前后端传输（`DefaultChatTransport` POST 全量历史、解析 SSE）；④ React 状态机（`useChat` 的 status/messages/parts 拼装）。手写要处理：SSE 拼帧缓冲、`data:` 行解析、`[DONE]` 终结、错误帧、逐 delta 状态更新——进阶 3.2 有完整手写实现。

加分点：补一句"我在 Go 侧手写过 SSE 客户端解析（第 3 章），SDK 内部做的事情我能逐层讲出来"——从黑盒使用者变成理解边界的使用者。

**Q3：流式 UI 的体验取舍——首字延迟 vs 完整校验，怎么权衡？**

标准回答：流式把**感知延迟**降到首 token 时间，长回答的体感差异是数量级的；代价是输出不再是"全量校验后才可见"——三种典型张力：① 结构化输出（JSON）流式途中是半个 JSON，不能直接 parse，要累积后校验或边流边做容错解析；② 内容审核/敏感词过滤需要缓冲窗口，流式要么延迟首字做前置审核、要么先播后审承担撤回成本；③ 流中途失败时部分内容已渲染，UI 策略要明确：保留已渲染内容 + 错误提示 + 一键重试（本项目 `page.tsx:87` 展示 `error`，`route.ts:292-295` 的 `onError` 把错误帧塞进流里）。

加分点：主动提"引用先出"——本项目让 data part 先于文本到达，用户先看到资料来源再看着答案逐字生成，信任感是设计出来的；再补一句 markdown 流式渲染的半成品问题（代码块围栏未闭合时的渲染策略）说明你真做过前端。

**Q4：引用怎么做到可点击跳回原文？**

标准回答：四个环节环环相扣，缺一不可——① 入库时 chunk 带溯源 metadata（`source` 文件名 + `chunk` 序号，`ingest/route.ts:146`）；② 检索命中后映射成 `SourceItem[]`，作为 data part 随流下发（`chat/route.ts:237-243, 279`）；③ system prompt 里的资料编号与 sources 数组顺序一致，模型回答里的 `[N]` 才有意义（`chat/route.ts:70-84`）；④ 前端卡片按数组顺序编号（不按 score 重排），点击展开 `text` 全文（`sources-card.tsx:56-111`）。

追问："真实产品里的'跳回原文'比这复杂在哪？" → 学习项目展开的是 chunk 全文；真实产品是 PDF 页码/段落锚点跳转 + 原文高亮——需要入库时记录页码和偏移，扫描件还要 OCR + 坐标信息，是另一条技术路线。

**Q5：入库做到一半失败了（比如 embedding 调用成功、落盘失败），一致性怎么处理？**

标准回答：本项目分三层——① `Store.add` 是 all-or-nothing（先整批校验再统一追加），内存里不会留"一半入库"的中间态（`vectorstore.ts:114-138`）；② `save` 原子写（临时文件 + rename），磁盘上任意时刻是完整的旧版或新版（:191-206）；③ 整个组装包在 try/catch 里，任一步失败返回 500、客户端重试安全（`ingest/route.ts:158-163`）。已知残余窗口：add 成功而 save 失败时，内存已更新但磁盘是旧版，进程重启后这部分丢失——应答里 `save` 成功才返回 `ok: true`，把"未持久化"显性化为失败，调用方重传即可（重传产生重复块，回到 Q1 的去重讨论）。

加分点：主动说出生产方案——先写可靠存储（对象存储/数据库）再更新索引，两步用幂等键关联；失败进重试队列；检索侧读存储为准而非内存为准。这说明你知道"学习项目的简化在哪、生产补什么"。

---

## 五、常见坑

1. **pnpm 与 node 版本不兼容（本机实踩）**：系统 node v22.12.0 与 pnpm 11.17 不兼容，`pnpm dev` 直接报错。修复：本项目所有 pnpm 命令前加 `PATH=/opt/homebrew/opt/node/bin:$PATH`（切到 homebrew 的 node v26）。教训：TS 工具链对 node 版本敏感，环境问题先 `node -v && pnpm -v` 确认，别怀疑代码。
2. **环境变量泄漏到客户端**：Next.js 里带 `NEXT_PUBLIC_` 前缀的 env 会被**内联进客户端 bundle**，任何用户在 devtools 里都能看到。密钥一律用无前缀变量、只在 Route Handler / 服务端的 `process.env` 读取（本项目三个 key 均如此）。事故形态：把 key 加了前缀图方便在前端用，等于把 key 公开发布。
3. **流式中的错误处理**：HTTP 状态码只能在流开始前返回；流式中途失败（模型超时、上游断开）时，只能把错误作为"错误帧"塞进流里（`route.ts:292-295` 的 `onError` 返回一句话给前端，细节留服务端日志）。前端要处理"部分内容已渲染 + 错误提示"的状态（`page.tsx:87`），且 fetch 流没有自动重连——要不要重试、重试是否重发整段，是产品决策不是默认行为。
4. **UIMessage 的文本不在顶层字段**：AI SDK v5+ 的 `UIMessage` 文本在 `parts` 数组里（混着 data part），服务端取 query（`route.ts:214-218`）和前端渲染（`page.tsx:75-81`）都要按 `part.type === "text"` 过滤；还按旧版 `message.content` 的直觉写会拿到 `undefined` 且不一定报错。
5. **`createOpenAI` 直接调用默认走 Responses API**：DeepSeek 等"OpenAI 兼容"服务只兼容 Chat Completions，必须 `.chat()`（`route.ts:43-52`）——否则报的是底层协议错误，很难第一时间想到是调用形态错了。
6. **dev 热重载清掉模块级状态**：Next.js dev 模式热重载会重执行模块，普通模块级变量被重置——进程级状态挂 `globalThis`（`kb.ts:25-41`）。但别把这个惯用法当银弹：serverless / 多实例下它同样不成立（进阶 3.3）。
7. **mock 遮住的常量错误：硅基流动端点少一个 `s`**（仓库现状）。`lib/embed.ts:62` 的 `SILICONFLOW_EMBEDDINGS_URL` 写成了 `.../v1/embedding`，正确是 `.../v1/embeddings`。mock 模式永远不碰这个常量，只有换真实 key 跑练习 6 才 404——**mock 能验证通路，验证不了常量**。防御：mock 跑通后至少用真实 key 冒烟一次。（读到这里建议顺手把这个常量改掉。）
8. **多余 import 是 lint 缺位的信号**（仓库现状）。`app/api/ingest/route.ts:17` 与 `app/api/chat/route.ts:39` 各有一行 `import { error } from "node:console"`——IDE 自动补全的误导入，不影响运行，但说明提交链路上没有 lint 把关。防御：`pnpm typecheck` 之外再接 ESLint（`next lint`），让这类残留在提交前被拦下。

---

## 六、动手练习

本章对应阶段二练习 6-7，是项目 2 的主体。骨架、详细任务说明和提示都在代码的 `TODO(练习N)` 块里，**先读 TODO 再动手**。

动手前一页速查（命令在 `stage-02-kb-agent/` 下执行）：

| 命令 | 作用 |
| --- | --- |
| `pnpm dev` | 开发模式，热重载 |
| `pnpm build` / `pnpm start` | 生产构建 / 生产启动 |
| `pnpm typecheck` | `tsc --noEmit` 全量类型检查——改完任何 TS 文件后必跑 |
| `pnpm eval` | `tsx scripts/eval.ts`，跑检索评估（第 6 章的指标） |

| 环境变量 | 作用 |
| --- | --- |
| `DEEPSEEK_API_KEY` | DeepSeek 生成模型 key（练习 7 问答用） |
| `SILICONFLOW_API_KEY` | 硅基流动 key（练习 6 embedding 用，bge-m3 1024 维） |
| `EMBEDDING_MOCK=1` | embedding 走确定性假向量，无 key 跑通 ingest 全链路 |
| `CHAT_MOCK=1` | 聊天模型换 MockLanguageModelV3，前端联调零成本 |

mock 变量只替换外部依赖那一层，检索管线、SSE 协议、引用下发都是真的——这就是它们存在的意义：把"能不能跑通"和"有没有 key"解耦。

**练习 6：ingest pipeline（写入路径）**

- 位置：`app/api/ingest/route.ts` 的 `TODO(练习6)`（组装逻辑，:94-129），依赖 `lib/chunk.ts` 与 `lib/embed.ts` 的两个同编号 TODO；
- 任务一句话：把解析出的文本走完 `chunk → embedTexts → 组装 Document（id 用 文件名#序号，metadata 带 source/chunk）→ add → save → 返回统计`；
- 验收（mock 模式无需任何 key）：

```bash
cd stage-02-kb-agent
PATH=/opt/homebrew/opt/node/bin:$PATH EMBEDDING_MOCK=1 pnpm dev
# 另开终端：
printf '# 指南\n\n第一段……\n\n第二段……\n' > /tmp/sample.md
curl -F "file=@/tmp/sample.md" http://localhost:3000/api/ingest
# 期望：{"ok":true,"chunks":N,...} 且 N > 0；生成 data/kb.json，内含 N 条 1024 维向量
```

- 参考答案：`docs/solutions/stage-02/exercise-6-ingest-pipeline.md`（含 PDF 支持进阶实现，**完成后再看**）。

**练习 7：问答 UI + 引用卡片（查询路径）**

- 位置：`app/api/chat/route.ts` 的 `TODO(练习7)`（检索 → 组装防幻觉 system prompt，:155-202）与 `components/sources-card.tsx` 的 `TODO(练习7)`②（引用卡片渲染）；
- 任务一句话：取最后一轮 user query → embed → top-k 检索 → sources 走 data part 下发 + 编号资料拼进 system prompt；前端把 `data-sources` 渲染成编号卡片，点击展开全文；
- 验收：

```bash
PATH=/opt/homebrew/opt/node/bin:$PATH EMBEDDING_MOCK=1 CHAT_MOCK=1 pnpm dev
# curl 验证协议（能看到先 data-sources 后 text-delta 的 SSE 流）：
curl -N -X POST localhost:3000/api/chat -H 'Content-Type: application/json' \
  -d '{"messages":[{"id":"1","role":"user","parts":[{"type":"text","text":"什么是 RAG？"}]}]}'
# 浏览器打开 http://localhost:3000：上传 md → 提问 → 流式回答 → 引用卡片可展开/收起
```

- 全链路验收标准：上传一份 md → 流式问答正常 → 回答带 `[N]` 编号 → 引用卡片可展开且内容与编号对应。有真实 key 后关掉两个 mock 再跑一遍；
- 参考答案：`docs/solutions/stage-02/exercise-7-chat-ui.md`（完成后再看）。

---

## 本章小结

- 项目 2 = 第 5 章 RAG 链路的产品化：写入路径（上传→chunk→embed→入库→落盘）与查询路径（检索→防幻觉 prompt→流式生成）共享一个进程级向量库单例。
- AI SDK 流式协议的核心抽象是 part：模型"说"的走 text part，服务端"知道"的走 data part——引用来源走 data part 是本项目最重要的协议设计。
- `useChat` 流式渲染的本质是"高频 setState"；SSE 是语言中立协议，浏览器侧消费与第 3 章 Go 侧解析同构。
- TS 版 RAG 底座与 Go 版逐行同构，差异清单（float32/number、稳定排序、error/throw、码点/rune）本身就是面试素材。
- mock 模式 = 测试替身思想：只替换最外层依赖（embedding、LLM），管线全真，无 key 也能端到端联调。
- 工程纪律复用：index 归位、维度校验、all-or-nothing 入库、原子落盘、服务端-only 密钥、流式错误帧——和第 4-5 章是同一套纪律的第二次出场。

下一章：[第 8 章：Go 并发编排与 worker pool](08-go-concurrency-and-worker-pool.md)——回到 Go，为多 Agent 编排系统打底：errgroup、context 预算与 semaphore 限流。
