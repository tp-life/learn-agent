/**
 * app/api/chat/route.ts —— RAG 查询路径的 HTTP 入口：流式问答 + 引用来源下发。
 *
 * 全链路（对照阶段文档 3.3 时序图的"查询路径"）：
 *
 *   前端 useChat POST 全量 messages
 *     → 取最后一轮 user query
 *     → embed(query) → 向量库 top-k（TODO 练习7①）
 *     → 检索结果拼进 system prompt（防幻觉 prompt）
 *     → DeepSeek streamText 流式生成
 *     → SSE 流回前端：回答文本 + data part（引用来源）
 *
 * 两个关键设计（面试考点）：
 *
 * 1. 对话历史即状态：本接口是无状态的，客户端每次把全量 messages 发过来，
 *    服务端不存会话。UI 消息（UIMessage，带 parts）要通过
 *    convertToModelMessages 转成模型消息（ModelMessage）才能喂给 LLM。
 *
 * 2. 引用来源走 data part 而不是塞进回答文本：来源是结构化数据，
 *    让模型把来源"说"出来既浪费 token 又不可控（模型可能漏报、错报编号）；
 *    由服务端直接随流推给前端，前端拿到的是类型化数据，想怎么渲染都行。
 *
 * 安全边界：messages 来自公网，只做"是不是数组"的基本校验；
 * system prompt 由服务端组装（用户输入只进 user 消息），
 * 防止客户端直接篡改 system prompt 绕过防幻觉约束。
 */

import {
  convertToModelMessages,
  createUIMessageStream,
  createUIMessageStreamResponse,
  streamText,
  type LanguageModel,
} from "ai";
import { createOpenAI } from "@ai-sdk/openai";
import { MockLanguageModelV3, simulateReadableStream } from "ai/test";
import { NextResponse } from "next/server";
import type { KbUIMessage, SourceItem } from "@/lib/chat-types";
import { error } from "node:console";
import { getKbStore } from "@/lib/kb";
import { embedTexts } from "@/lib/embed";

/**
 * DeepSeek 走 OpenAI 兼容协议：createOpenAI 换个 baseURL 即可。
 * 用 .chat() 而不是直接调用 provider——后者默认走 OpenAI Responses API，
 * DeepSeek 只兼容 Chat Completions API，这是接第三方"OpenAI 兼容"
 * 服务时最常见的坑。
 */
const deepseek = createOpenAI({
  baseURL: "https://api.deepseek.com",
  apiKey: process.env.DEEPSEEK_API_KEY,
});
const CHAT_MODEL = "deepseek-chat";

/** 检索返回的块数（top-k）。调它会影响 recall，是练习 8-9 的调参位之一。 */
const TOP_K = 3;

const NO_KB_SYSTEM =
  "你是一个知识库问答助手，当前知识库为空，没有任何可查的资料," +
  "请如实告知用户：知识库还没有内容，请先上传文档，不要凭自己的知识回答。";

/**
 * CHAT_MOCK=1 时的罐装回答。故意带上 [1] 编号，
 * 让"回答标注来源编号"的渲染效果可以离线验证。
 */
const CANNED_ANSWER =
  "（罐装回答）根据资料 [1]，当前处于 CHAT_MOCK 模式：LLM 被替换为确定性假模型，" +
  "但请求解析、流式协议、引用下发的整条管线都是真的。";

function buildRagSystemPrompt(sources: SourceItem[]): string {
  const blocks = sources
    .map((s, i) => `[${i + 1}]（来源：${s.source} 第${s.chunk}块）\n${s.text}`)
    .join("\n\n");
  return [
    "你是一个知识库问答助手，下面是按相关度排序的检索资料，编号即引用编号。",
    "",
    blocks,
    "",
    "回答要求：",
    "1. 仅根据资料回答，不要动用资料之外的知识；",
    "2. 资料不足以回答时，明确说「根据现有资料无法回答」，不要编造；",
    "3. 回答中用到资料时，在句末标注来源编号，如 [1] [2]",
  ].join("\n");
}

/**
 * mockChatModel 返回一个"确定性假 LLM"（可测试性教学点，与 EMBEDDING_MOCK 同源）：
 * 用 AI SDK 自带的 MockLanguageModelV3 替换 DeepSeek，每次输出同一段罐装文本。
 * 注意 mock 只发生在"模型"这一层——流式协议（SSE）、data part、前端渲染
 * 全部走真实代码路径，所以 mock 模式下联调通过 ≈ 真实模式只剩换 key。
 *
 * simulateReadableStream 按 chunkDelayInMs 逐块推送，模拟真实模型的打字机效果。
 */
function mockChatModel(): LanguageModel {
  // 把罐装文本按 ~12 字切片，模拟逐 token 流式输出。
  const pieces: string[] = [];
  for (let i = 0; i < CANNED_ANSWER.length; i += 12) {
    pieces.push(CANNED_ANSWER.slice(i, i + 12));
  }
  const deltas = pieces.map((delta) => ({
    type: "text-delta" as const,
    id: "text-1",
    delta,
  }));
  return new MockLanguageModelV3({
    doStream: async () => ({
      stream: simulateReadableStream({
        initialDelayInMs: 50,
        chunkDelayInMs: 60,
        chunks: [
          { type: "text-start", id: "text-1" },
          ...deltas,
          { type: "text-end", id: "text-1" },
          {
            type: "finish",
            finishReason: { unified: "stop", raw: undefined },
            usage: {
              inputTokens: {
                total: 0,
                noCache: 0,
                cacheRead: 0,
                cacheWrite: 0,
              },
              outputTokens: {
                total: CANNED_ANSWER.length,
                text: CANNED_ANSWER.length,
                reasoning: 0,
              },
            },
          },
        ],
      }),
    }),
  });
}

export async function POST(req: Request) {
  // —— 解析请求体：AI SDK 前端默认 POST { messages: UIMessage[] } ——
  let body: { messages?: KbUIMessage[] };
  try {
    body = await req.json();
  } catch {
    return NextResponse.json({ error: "请求体不是合法 JSON" }, { status: 400 });
  }
  const messages = body.messages;
  if (!Array.isArray(messages) || messages.length === 0) {
    return NextResponse.json(
      { error: "缺少 messages（UIMessage 数组）" },
      { status: 400 },
    );
  }

  const chatMock = process.env.CHAT_MOCK === "1";

  // TODO(练习7): 检索 → 组装 system prompt（RAG 查询路径的核心）
  //
  // 【任务】把下面的占位逻辑替换为真实的"检索增强"：
  //
  //   1. 取最后一轮 user query：
  //      const last = messages[messages.length - 1];
  //      从 last.parts 里收集 type === "text" 的部分拼出查询文本
  //      （UIMessage 的文本在 parts 里，不是顶层字段——易踩坑）。
  //      最后一条不是 user 消息或查不到文本时返回 400。
  //
  //   2. 检索：embedTexts([query])（lib/embed.ts）→ 取向量 →
  //      getKbStore()（lib/kb.ts）.search(vector, TOP_K) 得到 hits。
  //      知识库为空（size === 0）时不要报错：sources 为空数组、
  //      prompt 里说明"当前没有资料"，让模型回答"不知道"即可——
  //      "没有资料"和"出错了"是两回事，别用 500 表达前者。
  //
  //   3. 组装 sources（SourceItem[]）：从 hits 里映射
  //      { id, source: metadata.source, chunk: metadata.chunk,
  //        text, score }——metadata 是练习 6 入库时写入的溯源信息。
  //
  //   4. 组装防幻觉 system prompt，建议结构：
  //      - 角色：知识库问答助手；
  //      - 资料块：每个 hit 以 [编号] 开头（编号从 1 开始，与 sources
  //        数组顺序一致），后附来源文件名与块全文；
  //      - 三条硬指令：仅根据资料回答；资料不足就明说"不知道"，
  //        不要编造；回答中引用资料时标注来源编号（如 [1] [2]）。
  //      把这三个变量（system / sources）赋值后，下方流式管线不用改。
  //
  // 【提示】
  //   - 写入和查询必须用同一个 embedding 模型（lib/embed.ts 保证了这点），
  //     向量空间才对齐——这是 RAG 最隐蔽的 bug 来源之一。
  //   - 检索可能抛异常（embedding 网络失败等）：用 try/catch 兜住返回
  //     500 + 错误信息，别让客户端看到无信息的裸错误。
  //   - 防幻觉 prompt 的关键不是"措辞礼貌"，而是给模型一条
  //     "允许说不知道"的退路——没有这条，模型倾向于硬答（幻觉）。
  //   - CHAT_MOCK 模式也应该走你的检索代码：mock 只替换 LLM（见下），
  //     检索管线保持真实，才能把 mock 当集成测试用。
  //     骨架期的 CANNED_SOURCES 是联调前端用的占位，实现后删除。
  //
  // 【验收】
  //   EMBEDDING_MOCK=1 CHAT_MOCK=1 pnpm dev 启动后：
  //   curl -N -X POST localhost:3000/api/chat -H 'Content-Type: application/json' \
  //     -d '{"messages":[{"id":"1","role":"user","parts":[{"type":"text","text":"什么是 RAG？"}]}]}'
  //   能看到 SSE 流：先 data-sources（检索结果），再 text-delta 逐块流出。
  //   有真实 DEEPSEEK_API_KEY + SILICONFLOW_API_KEY 时关掉两个 mock，
  //   上传文档后提问，回答应基于文档内容并带 [N] 编号。
  //
  // 参考答案：docs/solutions/stage-02/exercise-7-chat-ui.md（完成后再看）

  // —— 以下为骨架已实现部分：选模型 + 流式响应管线 ——

  const last = messages[messages.length - 1];
  if (last.role !== "user") {
    return NextResponse.json(
      { error: "最后一条消息必须是 user" },
      { status: 400 },
    );
  }

  const query = last.parts
    .filter((p) => p.type === "text")
    .map((p) => p.text)
    .join("\n")
    .trim();
  if (query === "") {
    return NextResponse.json(
      { error: "用户消息没有文本内容" },
      { status: 400 },
    );
  }

  let system: string;
  let sources: SourceItem[] = [];
  const store = getKbStore();

  if (store.size === 0) {
    system = NO_KB_SYSTEM;
  } else {
    try {
      const [queryVector] = await embedTexts([query]);
      const hits = store.search(queryVector, TOP_K);

      sources = hits.map((h) => ({
        id: h.doc.id,
        source: h.doc.metadata.source ?? h.doc.id,
        chunk: h.doc.metadata.chunk ?? "?",
        text: h.doc.text,
        score: h.score,
      }));

      system = buildRagSystemPrompt(sources);
    } catch (err) {
      return NextResponse.json(
        {
          error: `检索失败: ${err instanceof Error ? err.message : String(err)}`,
        },
        { status: 500 },
      );
    }
  }

  // mock 只替换模型这一层；真实模式缺 key 时尽早报错，别等流开始了才挂。
  let model: LanguageModel;
  if (chatMock) {
    model = mockChatModel();
  } else {
    if (!process.env.DEEPSEEK_API_KEY) {
      return NextResponse.json(
        {
          error:
            "DEEPSEEK_API_KEY 未设置（没有 key 时可设 CHAT_MOCK=1 走 mock 路径）",
        },
        { status: 500 },
      );
    }
    model = deepseek.chat(CHAT_MODEL);
  }

  // createUIMessageStream 让我们能在模型流之外，往同一条 SSE 流里
  // 夹带自定义数据（这里是引用来源 data part）。
  const stream = createUIMessageStream<KbUIMessage>({
    execute: async ({ writer }) => {
      // 引用来源先于文本到达前端：data part 出现在助手消息的 parts 里，
      // 前端的引用卡片（TODO 练习7②）读的就是它。
      writer.write({ type: "data-sources", data: sources });
      const result = streamText({
        model,
        system,
        // UIMessage（前端格式，文本在 parts 里）→ ModelMessage（模型格式）。
        // 全量历史每轮都发给模型——LLM 本身无记忆，"对话"靠重放历史。
        // 注意 convertToModelMessages 是异步的（ai v7 起返回 Promise）。
        messages: await convertToModelMessages(messages),
      });
      writer.merge(result.toUIMessageStream());
    },
    // 不把服务端错误细节直接吐给客户端（可能含 key/内部路径），
    // 统一返回一句话，细节留在服务端日志。
    onError: (err) => {
      console.error("chat stream error:", err);
      return "生成回答时出错，请查看服务端日志";
    },
  });

  return createUIMessageStreamResponse({ stream });
}
