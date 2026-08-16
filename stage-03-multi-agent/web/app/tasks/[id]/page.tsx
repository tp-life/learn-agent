/**
 * app/tasks/[id]/page.tsx —— 任务详情页：子任务进度 + token 成本 + 人工审批。
 *
 * 这是练习8 前端部分的主战场。骨架提供：首屏加载 + 2s 轮询刷新 +
 * 子任务列表渲染（已可用，但既不实时也不能审批）。
 *
 * 练习：TODO(练习8)① 把轮询换成 EventSource 订阅 SSE；
 *       TODO(练习8)② waiting_human 子任务的审批交互。见下文标注。
 */

"use client";

import { useEffect, useState } from "react";
import { useParams } from "next/navigation";
import Link from "next/link";
import { getTask, type TaskDetail } from "@/lib/api";
import { StatusBadge } from "@/components/status-badge";

export default function TaskDetailPage() {
  // App Router 动态段：/tasks/t-123 → params.id = "t-123"。
  const { id } = useParams<{ id: string }>();
  const [task, setTask] = useState<TaskDetail | null>(null);
  const [error, setError] = useState<string | null>(null);

  // TODO(练习8)①: SSE 实时订阅 —— 用 EventSource 替换轮询
  //
  // 任务：把下面这个"首屏加载 + 2s setInterval 轮询"的 effect 换成
  // EventSource 订阅 `${API_BASE}/api/tasks/${id}/events`：
  //   - 首屏仍先 getTask 一次（SSE 连接建立前页面不能白屏）；
  //   - new EventSource(url)，onmessage 里 JSON.parse(e.data) 得到
  //     TaskDetail（与 getTask 同一个载荷结构），setTask 更新；
  //   - effect cleanup 里 es.close()——组件卸载/切换任务不关连接
  //     会泄漏（每进一次详情页多一条 SSE 长连接）；
  //   - 轮询 setInterval 整段删掉（SSE 接管刷新），但建议保留 onerror
  //     时的一次性 getTask 兜底（SSE 断线重连期间不丢状态）。
  //
  // 提示：
  //   - EventSource 是浏览器原生 API，不需要任何库；
  //     它自动重连、自动按 "data: ...\n\n" 分帧——阶段二 /api/chat
  //     手写过 SSE 解析，这次体会"浏览器帮你做完"的版本；
  //   - 跨源（:3000 → :8080）EventSource 同样受 CORS 约束，
  //     Go 侧的 withCORS 已经放开了；
  //   - React 18+ 的 StrictMode 开发模式下 effect 会跑两遍
  //     （mount → cleanup → mount），cleanup 里 es.close() 写对了就无害。
  //
  // 验收：npm run dev 起看板 + demo 模式 Go 服务，提交任务后详情页
  // 状态徽章约 1s 一跳（pending→running→waiting_human/done），
  // 不再依赖 2s 轮询节奏；React DevTools/网络面板能看到一条
  // events 长连接持续收帧。
  //
  // 参考答案：docs/solutions/stage-03/exercise-8-server-dashboard.md（完成后再看）
  useEffect(() => {
    getTask(id)
      .then(setTask)
      .catch((e) => setError(String(e)));
    const timer = setInterval(() => {
      getTask(id)
        .then(setTask)
        .catch(() => {});
    }, 2000);
    return () => clearInterval(timer);
  }, [id]);

  // TODO(练习8)②: 人工审批交互 —— waiting_human 子任务的 approve/reject
  //
  // 任务：给状态为 waiting_human 的子任务渲染审批区：
  //   - 高亮（橙边框/底色，配合 StatusBadge 让审批项一眼跳出来）；
  //   - 展示 prompt（审批人要看"它到底要干什么"才能做决定）；
  //   - "批准" / "驳回"两个按钮，点击调 lib/api 的 decide(id, sub.id,
  //     true/false, "dashboard-user")；按钮点击后 disable 防重复提交；
  //     decide 失败把错误显示出来；
  //   - 决定落盘后不需要手动刷新：服务端会自动触发 Resume 续跑，
  //     新的状态变化会通过 ① 的 SSE 推过来（这就是"事件驱动"的体感）。
  //
  // 提示：
  //   - 一个任务可能同时有多个子任务在等批，逐个渲染逐个批；
  //     服务端在"该任务没有待批项了"时才触发续跑；
  //   - "dashboard-user" 是演示用的审批人标识——审计必须留名（练习5
  //     的纪律在前端的落点）；真实产品里这里应是登录态用户名。
  //
  // 验收：demo 模式任务跑到 waiting_human 时，详情页出现高亮审批区；
  // 点"批准"后约 1s 内看到子任务继续执行直至任务 done；
  // 点"驳回"后子任务变 failed、任务按部分失败语义 done。
  //
  // 参考答案：docs/solutions/stage-03/exercise-8-server-dashboard.md（完成后再看）

  if (error) {
    return (
      <main style={{ maxWidth: 860, margin: "40px auto", fontFamily: "system-ui" }}>
        <div style={{ color: "#c00" }}>加载失败:{error}</div>
        <Link href="/">← 返回列表</Link>
      </main>
    );
  }
  if (!task) {
    return (
      <main style={{ maxWidth: 860, margin: "40px auto", fontFamily: "system-ui" }}>
        <div style={{ color: "#999" }}>加载中…</div>
      </main>
    );
  }

  return (
    <main
      style={{
        maxWidth: 860,
        margin: "40px auto",
        padding: "0 16px",
        fontFamily: "system-ui",
        display: "flex",
        flexDirection: "column",
        gap: 16,
      }}
    >
      <div>
        <Link href="/" style={{ color: "#1565c0" }}>
          ← 返回列表
        </Link>
      </div>
      <div style={{ display: "flex", alignItems: "center", gap: 12 }}>
        <h1 style={{ margin: 0, fontSize: 20 }}>{task.goal}</h1>
        <StatusBadge status={task.status} />
      </div>
      <div style={{ color: "#666", fontSize: 13 }}>
        任务 {task.id} · 累计 {task.total_tokens} tokens · 更新于{" "}
        {new Date(task.updated_at).toLocaleTimeString()}
      </div>

      <div style={{ display: "flex", flexDirection: "column", gap: 8 }}>
        {task.subtasks.map((s) => (
          <div
            key={s.id}
            style={{
              border: "1px solid #e0e0e0",
              borderRadius: 8,
              padding: "10px 14px",
              display: "flex",
              flexDirection: "column",
              gap: 6,
            }}
          >
            <div style={{ display: "flex", alignItems: "center", gap: 10 }}>
              <StatusBadge status={s.status} />
              <b>
                [{s.id}] {s.title}
              </b>
              <span style={{ color: "#999", fontSize: 12 }}>
                {s.tokens_used} tokens · 执行 {s.attempts} 次
                {s.requires_approval ? " · 高风险" : ""}
              </span>
            </div>
            {s.output && (
              <div style={{ color: "#444", fontSize: 13, whiteSpace: "pre-wrap" }}>
                {s.output}
              </div>
            )}
            {/* TODO(练习8)② 的审批区渲染在这里 */}
          </div>
        ))}
      </div>
    </main>
  );
}
