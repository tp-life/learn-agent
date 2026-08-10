/**
 * lib/chat-types.ts —— 聊天链路前后端共享的类型定义。
 *
 * 为什么单独一个文件：app/api/chat/route.ts（服务端）和 app/page.tsx（客户端）
 * 都要认识"引用来源"的数据结构；各自定义一份会漂移，抽出来保证
 * "服务端写出去的"和"前端读进来的"是同一个类型——这是 AI SDK 的
 * UIMessage 泛型设计鼓励的用法（类型参数贯穿 streamText → SSE → useChat）。
 *
 * 练习：本模块无需用户完成的部分（练习 7 的脚手架）。
 */

import type { UIMessage } from "ai";

/**
 * SourceItem 是一条"引用来源"：回答里 [N] 编号背后对应的检索命中块。
 *
 * 字段即"可溯源"的最小集合：来源文件名 + 块序号定位原文，
 * text 全文用于前端展开查看，score 帮助用户判断相关性强弱。
 */
export interface SourceItem {
  /** 文档在向量库里的 id（如 "guide.md#3"），同时是前端渲染的 key。 */
  id: string;
  /** 来源文件名（metadata.source）。 */
  source: string;
  /** 块序号（metadata.chunk）。 */
  chunk: string;
  /** 块全文：引用卡片折叠时只显示预览，点击展开看的就是它。 */
  text: string;
  /** 余弦相似度得分（[-1, 1]，越大越相关）。 */
  score: number;
}

/**
 * KbDataParts 声明本项目的"自定义数据部分"（data parts）。
 *
 * AI SDK 的流式协议里，助手消息不只有文本：服务端还可以通过
 * data part 把任意结构化数据随流推到前端，挂在消息的 parts 数组里
 * （类型名固定是 `data-${key}`）。检索到的引用来源既不是"回答文本"
 * 也不是"工具调用"，用 data part 传输是正解：
 *   服务端 writer.write({ type: "data-sources", data: SourceItem[] })
 *   前端   message.parts 里出现 { type: "data-sources", data: SourceItem[] }
 *
 * 注意用 type 而不是 interface：UIMessage 的泛型约束 UIDataTypes 带
 * 索引签名，interface 默认不满足（TS 只给 type 别名隐式索引签名）。
 */
export type KbDataParts = {
  sources: SourceItem[];
};

/**
 * KbUIMessage 是本项目的聊天消息类型：UIMessage 泛型的第二个参数
 * 挂上 KbDataParts 后，服务端 writer.write 和前端 parts 过滤都能获得
 * 类型检查（写错 data 结构直接编译报错，而不是运行时才现形）。
 */
export type KbUIMessage = UIMessage<unknown, KbDataParts>;
