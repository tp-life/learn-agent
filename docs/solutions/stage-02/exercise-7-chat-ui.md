# 练习 7 参考答案：问答界面（检索 → 防幻觉 prompt → 流式回答 + 引用卡片）

> 对应 TODO：`stage-02-kb-agent/app/api/chat/route.ts` 的 `TODO(练习7)`（检索 → 组装 system prompt）、
> `stage-02-kb-agent/components/sources-card.tsx` 的 `TODO(练习7)`（引用卡片）。
> **完成练习并自评后再看本文档。**
>
> 本文档代码已于 2026-08-06 实际粘贴进项目验证（同时临时套用了练习 6 参考答案，
> 因为检索依赖 embedTexts；验证后项目代码已全部恢复为骨架版）：
> - `pnpm build` 通过（Next.js 16.3.0，生产构建含类型检查全绿）；
> - `EMBEDDING_MOCK=1 CHAT_MOCK=1 pnpm dev` 实测：
>   - `curl -F "file=@sample.md" localhost:3100/api/ingest`（练习 6 管线）
>     → `{"ok":true,"chunks":1,"total":1}`；
>   - `curl -N -X POST localhost:3100/api/chat -d '{"messages":[{"id":"1","role":"user","parts":[{"type":"text","text":"什么是 RAG？"}]}]}'`
>     → SSE 流依次为：`data-sources`（真实检索到的块，含 source/chunk/text/score）、
>     `start`、`text-start`、8 个 `text-delta`（罐装回答逐块流出）、`text-end`、`finish`、`[DONE]`；
>   - 负路径：messages 为空 → 400；最后一条非 user → 400；user 消息无文本 → 400；
>   - 空知识库路径（删掉 data/kb.json 重启）：`data-sources` 为 `[]`，流式回答正常
>     （走 NO_KB_SYSTEM 分支，不报错）；
> - 前端引用卡片组件随 `pnpm build` 类型检查通过；SSE 中 `data-sources` 的结构与
>   `useChat<KbUIMessage>` 消费的 data part 格式一致（无浏览器人工点击验证，无头环境）。
> - 验证产生的 `data/kb.json` 已清理。

---

## 一、参考实现

### `app/api/chat/route.ts`（骨架只改三处，其余不变）

**① import 部分追加两行**（`type LanguageModel` 等原有 import 不动）：

```ts
import type { KbUIMessage, SourceItem } from "@/lib/chat-types";
import { embedTexts } from "@/lib/embed";
import { getKbStore } from "@/lib/kb";
```

**② 删除骨架期的 `PLACEHOLDER_SYSTEM` 与 `CANNED_SOURCES` 两个常量，替换为：**

```ts
/**
 * 知识库为空时的 system prompt：明确"没有资料"，
 * 让模型如实回答不知道，而不是硬答（幻觉）。
 */
const NO_KB_SYSTEM =
  "你是一个知识库问答助手。当前知识库为空，没有任何可查的资料。" +
  "请如实告知用户：知识库还没有内容，请先上传文档，不要凭自己的知识回答。";

/**
 * buildRagSystemPrompt 组装"资料块 + 防幻觉指令"的 system prompt。
 *
 * 防幻觉的关键设计（面试考点）：
 *  1. 资料块带 [N] 编号，编号与下发给前端的 sources 数组顺序一致——
 *     模型标注 [1] 和引用卡片 [1] 指向同一块，前后端编号同源；
 *  2. "仅根据资料回答"划清知识边界；
 *  3. 给模型一条"允许说不知道"的退路——没有这条，模型倾向于硬答。
 */
function buildRagSystemPrompt(sources: SourceItem[]): string {
  const blocks = sources
    .map((s, i) => `[${i + 1}]（来源：${s.source} 第 ${s.chunk} 块）\n${s.text}`)
    .join("\n\n");
  return [
    "你是一个知识库问答助手。下面是按相关度排序的检索资料，编号即引用编号。",
    "",
    blocks,
    "",
    "回答要求：",
    "1. 仅根据以上资料回答，不要动用资料之外的知识；",
    "2. 资料不足以回答时，明确说「根据现有资料无法回答」，不要编造；",
    "3. 回答中用到资料时，在句末标注来源编号，如 [1]、[2]。",
  ].join("\n");
}
```

**③ `TODO(练习7)` 块（含 `let system = PLACEHOLDER_SYSTEM; ... if (chatMock) {...}`）整段替换为：**

```ts
  // —— 检索 → 组装 system prompt（RAG 查询路径核心）——

  // 取最后一轮 user query：UIMessage 的文本在 parts 里，不是顶层字段。
  const last = messages[messages.length - 1];
  if (last.role !== "user") {
    return NextResponse.json({ error: "最后一条消息必须是 user" }, { status: 400 });
  }
  const query = last.parts
    .filter((p) => p.type === "text")
    .map((p) => p.text)
    .join("\n")
    .trim();
  if (query === "") {
    return NextResponse.json({ error: "用户消息没有文本内容" }, { status: 400 });
  }

  let system: string;
  let sources: SourceItem[] = [];
  const store = getKbStore();
  if (store.size === 0) {
    // "没有资料"不是错误：换一条如实说明的 prompt，让模型回答"不知道"。
    system = NO_KB_SYSTEM;
  } else {
    try {
      // 写入和查询用同一个 embedding 模型（lib/embed.ts 保证），向量空间才对齐。
      const [queryVector] = await embedTexts([query]);
      const hits = store.search(queryVector, TOP_K);
      // sources 的顺序 = hits 的顺序 = prompt 里 [N] 的编号顺序，三处同源。
      sources = hits.map((h) => ({
        id: h.doc.id,
        source: h.doc.metadata.source ?? h.doc.id,
        chunk: h.doc.metadata.chunk ?? "?",
        text: h.doc.text,
        score: h.score,
      }));
      system = buildRagSystemPrompt(sources);
    } catch (err) {
      // 检索失败（embedding 网络/鉴权等）：带错误信息的 500，而不是裸错误。
      return NextResponse.json(
        { error: `检索失败：${err instanceof Error ? err.message : String(err)}` },
        { status: 500 }
      );
    }
  }
```

### `components/sources-card.tsx`（完整替换骨架，此处给出全文）

```tsx
/**
 * components/sources-card.tsx —— 引用卡片：助手消息下方展示回答的依据来源。
 *
 * 为什么要有这个组件（不只是 UI 花哨）：引用 = 可验证性 = 用户信任 +
 * 幻觉兜底。用户能点开原文核对"模型是不是照资料答的"，
 * 这是 RAG 相对纯 LLM 问答的核心产品价值（阶段文档考点 Q7）。
 */

"use client";

import { useState } from "react";
import type { KbUIMessage } from "@/lib/chat-types";

/** 卡片折叠时的预览长度（按码点计，与 lib/chunk.ts 的码点纪律一致）。 */
const PREVIEW_CHARS = 80;

export function SourcesCard({ message }: { message: KbUIMessage }) {
  // 记录展开状态的卡片编号（Set 存下标；每次 setState 新建 Set 触发重渲染）。
  const [expanded, setExpanded] = useState<Set<number>>(new Set());

  // 服务端随流推来的 data part；类型由 KbUIMessage 泛型收窄为 SourceItem[]。
  const part = message.parts.find((p) => p.type === "data-sources");
  if (!part || part.data.length === 0) {
    return null;
  }
  const sources = part.data;

  const toggle = (i: number) => {
    setExpanded((prev) => {
      const next = new Set(prev);
      if (next.has(i)) {
        next.delete(i);
      } else {
        next.add(i);
      }
      return next;
    });
  };

  return (
    <div style={{ marginTop: 10, borderTop: "1px solid #ddd", paddingTop: 8 }}>
      <div style={{ fontSize: 12, color: "#888", marginBottom: 6 }}>
        资料来源（{sources.length}）· 点击卡片展开原文
      </div>
      <div style={{ display: "flex", flexDirection: "column", gap: 6 }}>
        {sources.map((s, i) => {
          const isOpen = expanded.has(i);
          // 按码点截取预览（emoji 代理对不会被切坏）。
          const chars = Array.from(s.text);
          const preview =
            chars.length > PREVIEW_CHARS
              ? chars.slice(0, PREVIEW_CHARS).join("") + "…"
              : s.text;
          return (
            <div
              key={s.id}
              onClick={() => toggle(i)}
              style={{
                cursor: "pointer",
                background: "#fff",
                border: "1px solid #e0e0e0",
                borderRadius: 6,
                padding: "6px 10px",
                fontSize: 13,
              }}
            >
              {/* 编号按数组顺序从 1 开始，与服务端 prompt 的 [N] 同源，
                  绝不按 score 重排——否则回答里的标注就对不上了。 */}
              <div style={{ fontWeight: 600 }}>
                [{i + 1}] {s.source} · 第 {s.chunk} 块 · 相关度 {s.score.toFixed(3)}
              </div>
              <div style={{ color: "#555", whiteSpace: "pre-wrap", marginTop: 2 }}>
                {isOpen ? s.text : preview}
              </div>
            </div>
          );
        })}
      </div>
    </div>
  );
}
```

## 二、关键设计点

1. **UIMessage 的文本在 `parts` 里，不是顶层字段**——这是 AI SDK v5+ 与旧版
   （`message.content` 字符串）的最大差异，也是本练习第一易错处：
   `last.parts.filter((p) => p.type === "text")` 才能拿到查询文本。
   同理，服务端喂模型前必须 `convertToModelMessages`（ai v7 起是异步的，
   漏写 await 会得到 `Promise<ModelMessage[]>` 的类型错误——骨架注释已标注）。

2. **编号同源是引用功能的命门**：prompt 里的 `[N]`、下发前端的 sources 数组、
   引用卡片的编号，三处必须来自同一个顺序（hits 降序）。任何一个环节重排
   （比如前端按 score 再排一次——hits 本来就是按 score 排的，重排多此一举
   还容易写出不稳定排序），模型回答里的 [1] 就和卡片 [1] 对不上，
   引用功能形同虚设。

3. **sources 走 data part 而不是让模型"说"出来**：让模型在回答末尾列来源，
   既费 token 又不可控（漏报、错报编号、编造文件名）；由服务端随流推结构化
   数据，前端拿到的是类型化的 `SourceItem[]`，渲染自由且必然准确。
   "回答里的编号标注"和"引用卡片"是两层：前者靠 prompt 指令（可能出错，
   但有退路），后者是服务端直出（必然正确）。

4. **空知识库不是错误**：`store.size === 0` 时返回 500 是典型的误用——
   用户什么都没做错。正确做法是换一条"没有资料"的 system prompt，
   让模型如实说"请先上传文档"。这本身就是防幻觉设计的一部分：
   没有资料时最差的响应是让模型凭参数知识硬答。

5. **CHAT_MOCK 只替换模型这一层**（与 EMBEDDING_MOCK 同思想）：mock 模式下
   检索管线是真实的——`MockLanguageModelV3` 只换掉 DeepSeek。这样 mock 模式
   联调通过 ≈ 真实模式只剩换 key，mock 才能当集成测试用。骨架期的
   `CANNED_SOURCES` 只是让你在做完 TODO① 之前就能联调卡片，实现后必须删——
   留着它，mock 模式就测不到你的检索代码。

6. ** `.chat()` vs 直接调用 provider**：`@ai-sdk/openai` 的 provider 默认走
   OpenAI Responses API，DeepSeek 只兼容 Chat Completions API，
   `deepseek.chat("deepseek-chat")` 显式选后者。接任何"OpenAI 兼容"的
   第三方服务都要先确认它兼容的是哪个 API。

## 三、对照清单

完成后逐条自评（不要求与答案一字不差，覆盖条目即可）：

- [ ] 从 `last.parts` 提取查询文本（不是 `last.content`）；最后一条非 user、
      无文本内容都返回 400
- [ ] 检索：embedTexts([query]) → `store.search(vector, TOP_K)`；
      检索异常被 try/catch 兜住返回带信息的 500
- [ ] 空知识库不报错，走"没有资料"的 prompt 分支
- [ ] system prompt 三要素齐全：资料块带 [N] 编号、"仅根据资料回答"、
      "不足就说不知道"+ 标注来源编号
- [ ] sources 数组顺序与 prompt 编号、卡片编号同源；卡片按数组顺序编号，
      不按 score 重排
- [ ] 引用卡片：编号 + 文件名 + 块序号 + 预览，点击展开/收起全文；
      无 sources 时不渲染
- [ ] 删除了骨架期的 CANNED_SOURCES 占位分支，CHAT_MOCK 下也走真实检索
- [ ] `pnpm build` 通过；mock 模式下 curl /api/chat 看到 SSE 里
      `data-sources` 先于 `text-delta` 到达
- [ ] 能口头回答：为什么 sources 不让模型自己说？为什么空库不该 500？
      编号同源指什么？UIMessage 和 ModelMessage 的区别与转换时机？
