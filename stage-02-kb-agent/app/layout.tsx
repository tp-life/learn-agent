/**
 * app/layout.tsx —— Next.js App Router 根布局。
 * 练习：本模块无需用户完成的部分（练习 7 做聊天 UI 时可按需美化）。
 */

import type { Metadata } from "next";
import type { ReactNode } from "react";

export const metadata: Metadata = {
  title: "知识库 Agent",
  description: "阶段二项目 2：全栈知识库 Agent（RAG 写入/查询 + Evals）",
};

export default function RootLayout({ children }: { children: ReactNode }) {
  return (
    <html lang="zh-CN">
      <body>{children}</body>
    </html>
  );
}
