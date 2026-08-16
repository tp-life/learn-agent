/**
 * app/layout.tsx —— Next.js App Router 根布局。
 * 练习：本模块无需用户完成的部分。
 */

import type { Metadata } from "next";
import type { ReactNode } from "react";

export const metadata: Metadata = {
  title: "多 Agent 编排看板",
  description: "阶段三项目 3：Go 编排引擎 + Next.js 实时看板",
};

export default function RootLayout({ children }: { children: ReactNode }) {
  return (
    <html lang="zh-CN">
      <body style={{ margin: 0 }}>{children}</body>
    </html>
  );
}
