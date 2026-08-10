/**
 * components/sources-card.tsx —— 引用卡片：助手消息下方展示回答的依据来源。
 *
 * 为什么要有这个组件（不只是 UI 花哨）：引用 = 可验证性 = 用户信任 +
 * 幻觉兜底。用户能点开原文核对"模型是不是照资料答的"，
 * 这是 RAG 相对纯 LLM 问答的核心产品价值（阶段文档考点 Q7）。
 *
 * 练习：本组件的渲染逻辑是 TODO(练习7)②，骨架期不渲染任何内容。
 */

"use client";

import type { KbUIMessage } from "@/lib/chat-types";

export function SourcesCard({ message }: { message: KbUIMessage }) {
  // TODO(练习7): 引用来源卡片（渲染 data part 里的检索来源）
  //
  // 【任务】服务端（app/api/chat/route.ts）已经通过 data part 把检索到的
  // 引用来源随流推了过来，本组件负责把它渲染成可查看的引用卡片：
  //
  //   1. 从 message.parts 里取出 sources：
  //      const part = message.parts.find((p) => p.type === "data-sources");
  //      找到则 part.data 是 SourceItem[]（类型已由 KbUIMessage 泛型保证）。
  //      没有该 part 或数组为空时返回 null（不渲染）。
  //
  //   2. 渲染成卡片列表，每条来源包含：
  //      - 编号 [N]（从 1 开始，与回答里的标注对应——顺序必须与
  //        服务端 system prompt 里的资料编号一致，所以按数组顺序编号，
  //        不要按 score 重排）；
  //      - 来源文件名 + 块序号（source、chunk 字段）；
  //      - chunk 内容预览（比如前 80 字 + 省略号）；
  //      - 相似度得分（可选展示，帮助判断相关性强弱）。
  //
  //   3. 点击卡片展开/收起 chunk 全文（text 字段）：
  //      用 useState 记录哪些卡片处于展开状态（比如 Set<number> 或
  //      当前展开的编号），点击切换。
  //
  // 【提示】
  //   - 样式从简： inline style 即可，参考 app/page.tsx 的风格。
  //   - 流式期间 sources 可能先到达、文本还在生成——卡片独立于文本渲染，
  //     天然支持这种"引用先出"的效果，不需要额外处理。
  //   - 调试技巧：想看原始数据，可以临时 console.log(message.parts)，
  //     或给 useChat 传 onData 回调观察每个 data part 的到达时机。
  //
  // 【验收】CHAT_MOCK=1 pnpm dev 启动（骨架期 route.ts 会下发罐装来源），
  // 页面上提问后：助手回答下方出现引用卡片，显示编号/文件名/预览，
  // 点击能展开全文，再点收起。完成 TODO① 后，卡片内容应来自真实检索结果。
  //
  // 参考答案：docs/solutions/stage-02/exercise-7-chat-ui.md（完成后再看）
  void message; // 骨架期避免未使用告警；实现后删除本行
  return null;
}
