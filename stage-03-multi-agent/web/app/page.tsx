/**
 * app/page.tsx —— 任务列表页：提交新任务 + 全任务状态总览。
 *
 * 客户端组件（"use client"）：提交表单与定时刷新都需要浏览器侧状态。
 * 列表页用 2s 轮询而不是 SSE——列表是低频总览，轮询够用且简单；
 * 需要逐子任务实时跟进的是详情页（app/tasks/[id]/page.tsx，练习8 的主战场）。
 *
 * 练习：本页已完整提供，无需学习者完成。
 */

"use client";

import { useEffect, useState } from "react";
import Link from "next/link";
import { createTask, listTasks, type TaskView } from "@/lib/api";
import { StatusBadge } from "@/components/status-badge";

export default function Home() {
  const [tasks, setTasks] = useState<TaskView[]>([]);
  const [goal, setGoal] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [submitting, setSubmitting] = useState(false);

  const refresh = () => {
    listTasks()
      .then((r) => setTasks(r.tasks))
      .catch((e) => setError(String(e)));
  };

  // 2s 轮询：列表页的低频刷新。注意 Go 服务没启动时 error 会常驻页面——
  // 这正是"先起 Go 服务再起看板"的提示位。
  useEffect(() => {
    refresh();
    const timer = setInterval(refresh, 2000);
    return () => clearInterval(timer);
  }, []);

  const onSubmit = async (e: React.FormEvent) => {
    e.preventDefault();
    const text = goal.trim();
    if (!text || submitting) return;
    setSubmitting(true);
    setError(null);
    try {
      await createTask(text);
      setGoal("");
      refresh(); // 提交后立刻刷一次，新任务马上出现在列表里
    } catch (err) {
      setError(String(err));
    } finally {
      setSubmitting(false);
    }
  };

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
      <h1 style={{ margin: 0 }}>多 Agent 编排看板</h1>
      <p style={{ color: "#666", margin: 0 }}>
        Go 编排引擎的实时看板。提交目标后 planner 分解子任务、worker 并发执行；
        高风险子任务会暂停等待人工审批（在详情页操作）。
      </p>

      <form onSubmit={onSubmit} style={{ display: "flex", gap: 8 }}>
        <input
          value={goal}
          onChange={(e) => setGoal(e.target.value)}
          placeholder="输入任务目标，如：写一份数据治理周报…"
          style={{ flex: 1, padding: "10px 12px", borderRadius: 8, border: "1px solid #ccc" }}
        />
        <button type="submit" disabled={submitting} style={{ padding: "10px 20px" }}>
          提交任务
        </button>
      </form>

      {error && <div style={{ color: "#c00" }}>出错了：{error}（Go 服务启动了吗？）</div>}

      <div style={{ display: "flex", flexDirection: "column", gap: 8 }}>
        {tasks.length === 0 && !error && (
          <div style={{ color: "#999" }}>暂无任务，提交一个试试。</div>
        )}
        {tasks.map((t) => (
          <Link
            key={t.id}
            href={`/tasks/${t.id}`}
            style={{ textDecoration: "none", color: "inherit" }}
          >
            <div
              style={{
                border: "1px solid #e0e0e0",
                borderRadius: 8,
                padding: "10px 14px",
                display: "flex",
                alignItems: "center",
                gap: 12,
              }}
            >
              <StatusBadge status={t.status} />
              <span style={{ flex: 1, overflow: "hidden", textOverflow: "ellipsis" }}>
                {t.goal}
              </span>
              <span style={{ color: "#999", fontSize: 12, whiteSpace: "nowrap" }}>
                {t.total_tokens} tokens
              </span>
            </div>
          </Link>
        ))}
      </div>
    </main>
  );
}
