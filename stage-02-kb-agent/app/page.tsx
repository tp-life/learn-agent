/**
 * app/page.tsx —— 问答界面：流式回答 + 引用卡片。
 *
 * 客户端组件（"use client"）：聊天是高度交互的场景，状态交给
 * AI SDK 的 useChat 管理。useChat 干的事（这个 hook 替代了我们手写的部分）：
 *   - 维护 messages 状态机（status: submitted → streaming → ready / error）；
 *   - sendMessage 时把全量 messages POST 到 /api/chat（对话历史即状态：
 *     服务端不存会话，每轮重放全部历史）；
 *   - 解析 SSE 流，把 text-delta 增量拼进助手消息的 parts——
 *     React 状态每收到一块就更新，于是 UI 呈现"打字机"效果。
 *     流式渲染的本质不是特殊技巧，就是"高频 setState"。
 *
 * 练习：本文件骨架已就绪；引用卡片区见 components/sources-card.tsx
 * 的 TODO(练习7)。
 */

"use client";

import { useState } from "react";
import { useChat } from "@ai-sdk/react";
import { DefaultChatTransport } from "ai";
import type { KbUIMessage } from "@/lib/chat-types";
import { SourcesCard } from "@/components/sources-card";

export default function Home() {
  // KbUIMessage 泛型把 data part 的类型带进 messages——
  // 下面过滤 parts 时 data-sources 的 data 是 SourceItem[]，有类型检查。
  const { messages, sendMessage, status, error } = useChat<KbUIMessage>({
    transport: new DefaultChatTransport({ api: "/api/chat" }),
  });
  const [input, setInput] = useState("");
  const busy = status === "submitted" || status === "streaming";

  const onSubmit = (e: React.FormEvent) => {
    e.preventDefault();
    const text = input.trim();
    if (!text || busy) return;
    // sendMessage 触发一次完整的"发消息 → 收流式回答"循环。
    sendMessage({ text });
    setInput("");
  };

  return (
    <main
      style={{
        maxWidth: 760,
        margin: "40px auto",
        padding: "0 16px",
        fontFamily: "system-ui",
        display: "flex",
        flexDirection: "column",
        gap: 16,
      }}
    >
      <h1 style={{ margin: 0 }}>知识库 Agent</h1>
      <p style={{ color: "#666", margin: 0 }}>
        基于已上传文档的问答（RAG 查询路径）。上传文档：
        <code>curl -F "file=@doc.md" localhost:3000/api/ingest</code>
      </p>

      <div style={{ display: "flex", flexDirection: "column", gap: 12 }}>
        {messages.map((m) => (
          <div
            key={m.id}
            style={{
              alignSelf: m.role === "user" ? "flex-end" : "flex-start",
              maxWidth: "90%",
              background: m.role === "user" ? "#e8f0fe" : "#f5f5f5",
              borderRadius: 8,
              padding: "10px 14px",
            }}
          >
            {/* UIMessage 的文本在 parts 数组里（可能混着 data part 等其他类型），
                逐 part 按类型渲染——这是 AI SDK v5+ 与旧版 message.content 的最大差异。 */}
            {m.parts.map((part, i) =>
              part.type === "text" ? (
                <span key={i} style={{ whiteSpace: "pre-wrap" }}>
                  {part.text}
                </span>
              ) : null
            )}
            {/* 助手消息下方挂引用卡片（TODO 练习7② 的实现位置） */}
            {m.role === "assistant" && <SourcesCard message={m} />}
          </div>
        ))}
        {busy && <div style={{ color: "#999" }}>思考中…</div>}
        {error && <div style={{ color: "#c00" }}>出错了：{error.message}</div>}
      </div>

      <form onSubmit={onSubmit} style={{ display: "flex", gap: 8 }}>
        <input
          value={input}
          onChange={(e) => setInput(e.target.value)}
          placeholder="向知识库提问…"
          style={{ flex: 1, padding: "10px 12px", borderRadius: 8, border: "1px solid #ccc" }}
        />
        <button type="submit" disabled={busy} style={{ padding: "10px 20px" }}>
          发送
        </button>
      </form>
    </main>
  );
}
